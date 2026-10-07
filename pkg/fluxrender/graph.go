package fluxrender

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/tobiash/gitops-preview-toolkit/pkg/build"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander/fluxks"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander/helm"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const maxRebuilds = 64

type helmEvaluation struct {
	expansion   plugin.Expansion
	diagnostics []plugin.Diagnostic
}

func cloneHelmEvaluations(source map[string]map[string]helmEvaluation) map[string]map[string]helmEvaluation {
	result := make(map[string]map[string]helmEvaluation, len(source))
	for trigger, evaluations := range source {
		result[trigger] = make(map[string]helmEvaluation, len(evaluations))
		for key, evaluation := range evaluations {
			evaluation.expansion.Resources = append([]plugin.Resource{}, evaluation.expansion.Resources...)
			evaluation.diagnostics = append([]plugin.Diagnostic{}, evaluation.diagnostics...)
			result[trigger][key] = evaluation
		}
	}
	return result
}

func (se *session) rememberHelm(trigger, key string, evaluation helmEvaluation) {
	evaluations := se.helmEvaluations[trigger]
	if evaluations == nil {
		evaluations = map[string]helmEvaluation{}
		se.helmEvaluations[trigger] = evaluations
	}
	// Bound histories when foreign inputs keep changing during one long session.
	if len(evaluations) >= maxRebuilds {
		clear(evaluations)
	}
	evaluations[key] = evaluation
}

// Every round replaces the complete producer set. Unlike a count-only discovery
// loop, this reevaluates unchanged identities when values or control specs change.
func (s *Service) expand(ctx context.Context, se *session, inputs []plugin.Resource) (*plugin.ExpandResponse, error) {
	foreign := []plugin.Resource{}
	for _, input := range inputs {
		if !se.owned[ownedIdentity{id: input.ID, provenance: input.Provenance}] {
			foreign = append(foreign, input)
		}
	}
	roots := []plugin.Expansion{}
	bootstrap := map[string]bool{}
	rootDiagnostics := []plugin.Diagnostic{}
	rootPaths := map[string]bool{}
	for _, path := range se.options.Paths {
		path = filepath.Clean(path)
		if rootPaths[path] {
			continue
		}
		rootPaths[path] = true
		dp := expander.DiscoveredPath{Path: path, Producer: "path " + path}
		built, err := s.buildPath(ctx, se, dp, func(build expander.DiscoveredPath) bool {
			key := pathKey(se.options.Root, build)
			if bootstrap[key] {
				return false
			}
			bootstrap[key] = true
			return true
		})
		if err != nil {
			return nil, err
		}
		resources, err := outputResources(built, plugin.Provenance{Kind: "Path", Path: path, Text: dp.Producer})
		if err != nil {
			return nil, err
		}
		roots = append(roots, plugin.Expansion{ID: "flux:path:" + path, Trigger: "root", Resources: resources})
		rootDiagnostics = append(rootDiagnostics, warnings(built)...)
	}
	previous := roots
	seen := map[string]bool{}
	lastDiagnostics := []plugin.Diagnostic{}
	for range maxRebuilds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current := reachable(foreign, previous)
		inventory, err := s.inventory(foreign, current)
		if err != nil {
			return nil, err
		}
		diagnostics := append([]plugin.Diagnostic{}, rootDiagnostics...)
		next := append([]plugin.Expansion{}, roots...)
		if se.git != nil {
			result, err := se.git.Expand(ctx, inventory)
			if err != nil {
				return nil, err
			}
			diagnostics = append(diagnostics, issues(result, "")...)
		}
		// Bootstrap aliases are consumed once per round and include the entire
		// transformed build context, not just its physical directory.
		aliases := map[string]bool{}
		for key := range bootstrap {
			aliases[key] = true
		}
		for _, control := range inventory.Resources() {
			gvk := control.GetGvk()
			isKS := gvk.Group == "kustomize.toolkit.fluxcd.io" && gvk.Kind == "Kustomization"
			if se.config.FluxKS != nil && !*se.config.FluxKS {
				isKS = false
			}
			isHR := gvk.Group == "helm.toolkit.fluxcd.io" && gvk.Kind == "HelmRelease" && se.config.Helm
			if !isKS && !isHR {
				continue
			}
			trigger, err := plugin.ParseResource([]byte(control.MustYaml()), plugin.Provenance{})
			if err != nil {
				return nil, err
			}
			inputKey := ""
			if isHR {
				inputKey, err = helmInputKey(control, inventory, se)
				if err != nil {
					return nil, err
				}
				if cached, ok := se.helmEvaluations[trigger.ID][inputKey]; ok {
					next = append(next, cached.expansion)
					diagnostics = append(diagnostics, cached.diagnostics...)
					continue
				}
			}
			diagnosticStart := len(diagnostics)
			// Evaluate one controller at a time, preserving all source and value
			// inputs. This prevents multi-release merges hiding output collisions.
			view := render.NewDefaultRender(s.log)
			for _, res := range inventory.Resources() {
				g := res.GetGvk()
				controller := (g.Group == "helm.toolkit.fluxcd.io" && g.Kind == "HelmRelease") ||
					(g.Group == "kustomize.toolkit.fluxcd.io" && g.Kind == "Kustomization")
				if !controller || res == control {
					if err := view.Append(res.DeepCopy()); err != nil {
						return nil, err
					}
				}
			}
			var engine expander.Expander
			kind := "kustomization"
			if isKS {
				ks := fluxks.NewExpander(s.log)
				if se.git != nil {
					ks = fluxks.NewExpanderWithResolver(s.log, se.git)
				}
				if se.options.StrictInputs {
					ks.SetStrictInputs()
				}
				engine = ks
			} else {
				kind = "helmrelease"
				hr := helm.NewExpanderWithAcquisitionCache(se.runner, nil, s.log, se.epoch.charts)
				if se.git != nil {
					hr = helm.NewExpanderWithAcquisitionCache(se.runner, se.git, s.log, se.epoch.charts)
				}
				if se.options.StrictInputs {
					hr.SetStrictInputs()
				}
				engine = hr
			}
			result, err := engine.Expand(ctx, view)
			expansion := plugin.Expansion{ID: "flux:" + kind + ":" + trigger.ID, Trigger: trigger.ID, Resources: []plugin.Resource{}}
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				diagnostics = append(diagnostics, diagnostic("render-error", err, trigger.ID))
				if isHR {
					se.rememberHelm(trigger.ID, inputKey, helmEvaluation{expansion: expansion,
						diagnostics: append([]plugin.Diagnostic{}, diagnostics[diagnosticStart:]...)})
				}
				next = append(next, expansion)
				continue
			}
			diagnostics = append(diagnostics, issues(result, trigger.ID)...)
			p := plugin.Provenance{Kind: control.GetKind(), Name: control.GetName(), Namespace: control.GetNamespace()}
			for _, path := range result.DiscoveredPaths {
				built, err := s.buildPath(ctx, se, path, func(build expander.DiscoveredPath) bool {
					key := pathKey(se.options.Root, build)
					if aliases[key] {
						delete(aliases, key)
						return false
					}
					return true
				})
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					diagnostics = append(diagnostics, diagnostic("render-error", err, trigger.ID))
					continue
				}
				resources, err := outputResources(built, p)
				if err != nil {
					return nil, err
				}
				expansion.Resources = append(expansion.Resources, resources...)
				diagnostics = append(diagnostics, warnings(built)...)
			}
			if result.Resources != nil {
				resources, err := outputResources(result.Resources, p)
				if err != nil {
					return nil, err
				}
				expansion.Resources = append(expansion.Resources, resources...)
			}
			next = append(next, expansion)
			if isHR {
				se.rememberHelm(trigger.ID, inputKey, helmEvaluation{expansion: expansion,
					diagnostics: append([]plugin.Diagnostic{}, diagnostics[diagnosticStart:]...)})
			}
		}
		next = reachable(foreign, next)
		diagnostics = append(diagnostics, collisions(foreign, next)...)
		canonicalize(next, diagnostics)
		lastDiagnostics = diagnostics
		fingerprint := digest(next)
		if fingerprint == digest(previous) {
			return &plugin.ExpandResponse{Expansions: next, Diagnostics: diagnostics}, nil
		}
		if seen[fingerprint] {
			diagnostics = append(diagnostics, diagnostic("oscillation", fmt.Errorf("flux local closure oscillates"), ""))
			return &plugin.ExpandResponse{Expansions: next, Diagnostics: diagnostics}, nil
		}
		seen[fingerprint] = true
		previous = next
	}
	lastDiagnostics = append(lastDiagnostics, diagnostic("iteration-limit", fmt.Errorf("flux local closure exceeded %d rebuilds", maxRebuilds), ""))
	return &plugin.ExpandResponse{Expansions: previous, Diagnostics: lastDiagnostics}, nil
}

// Only referenced input objects invalidate a Helm evaluation. Unrelated desired
// output (including a release's random-generated Secrets) is not a values input.
func helmInputKey(control *resource.Resource, inventory *render.Render, se *session) (string, error) {
	object, err := control.Map()
	if err != nil {
		return "", err
	}
	type reference struct{ group, kind, namespace, name string }
	references := map[reference][]string{}
	source, _, _ := unstructured.NestedMap(object, "spec", "chart", "spec", "sourceRef")
	ns, _ := source["namespace"].(string)
	if ns == "" {
		ns = control.GetNamespace()
	}
	kind, _ := source["kind"].(string)
	name, _ := source["name"].(string)
	references[reference{group: "source.toolkit.fluxcd.io", kind: kind, namespace: ns, name: name}] = []string{}
	values, _, _ := unstructured.NestedSlice(object, "spec", "valuesFrom")
	for _, value := range values {
		ref, ok := value.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		key, _ := ref["valuesKey"].(string)
		if key == "" {
			key = "values.yaml"
		}
		identity := reference{kind: kind, namespace: control.GetNamespace(), name: name}
		references[identity] = append(references[identity], key)
	}
	// Reconciliation status and unrelated metadata are not rendering inputs.
	inputs := []any{object["spec"]}
	if kind == "GitRepository" && se.git != nil {
		path, found := se.git.ResolvePath(ns, name)
		inputs = append(inputs, fmt.Sprintf("source:%t:%s", found, path))
	}
	for _, res := range inventory.Resources() {
		identity := reference{group: res.GetGvk().Group, kind: res.GetKind(), namespace: res.GetNamespace(), name: res.GetName()}
		keys, relevant := references[identity]
		if !relevant {
			continue
		}
		object, err := res.Map()
		if err != nil {
			return "", err
		}
		if identity.group != "" {
			inputs = append(inputs, object["spec"])
			continue
		}
		values := map[string]any{}
		for _, key := range keys {
			for _, field := range []string{"data", "binaryData", "stringData"} {
				data, _, _ := unstructured.NestedFieldNoCopy(object, field, key)
				values[field+"/"+key] = data
			}
		}
		inputs = append(inputs, values)
	}
	return digest(inputs), nil
}

func (s *Service) inventory(foreign []plugin.Resource, expansions []plugin.Expansion) (*render.Render, error) {
	r := render.NewDefaultRender(s.log)
	add := func(input plugin.Resource) error {
		// Unnamed desired objects are terminal preview outputs, not named
		// controller inputs or valuesFrom lookup candidates.
		if input.Logical {
			return nil
		}
		parsed, err := resmap.NewFactory(resource.NewFactory(nil)).NewResMapFromBytes([]byte(input.YAML))
		if err != nil {
			return fmt.Errorf("inventory %s: %w", input.ID, err)
		}
		return r.AbsorbAll(parsed)
	}
	for _, input := range foreign {
		if err := add(input); err != nil {
			return nil, err
		}
	}
	for _, expansion := range expansions {
		for _, input := range expansion.Resources {
			if err := add(input); err != nil {
				return nil, err
			}
		}
	}
	r.Sort()
	return r, nil
}

// reachable drops orphaned producer subgraphs, including self-supporting cycles.
func reachable(foreign []plugin.Resource, expansions []plugin.Expansion) []plugin.Expansion {
	ids := map[string]bool{}
	for _, input := range foreign {
		ids[input.ID] = true
	}
	accepted := map[string]bool{}
	for {
		changed := false
		for _, expansion := range expansions {
			if accepted[expansion.ID] || (expansion.Trigger != "root" && !ids[expansion.Trigger]) {
				continue
			}
			accepted[expansion.ID] = true
			changed = true
			for _, resource := range expansion.Resources {
				ids[resource.ID] = true
			}
		}
		if !changed {
			break
		}
	}
	result := []plugin.Expansion{}
	for _, expansion := range expansions {
		if accepted[expansion.ID] {
			result = append(result, expansion)
		}
	}
	return result
}

func digest(value any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func canonicalize(expansions []plugin.Expansion, diagnostics []plugin.Diagnostic) {
	for _, expansion := range expansions {
		sort.Slice(expansion.Resources, func(i, j int) bool { return expansion.Resources[i].ID < expansion.Resources[j].ID })
	}
	sort.Slice(expansions, func(i, j int) bool { return expansions[i].ID < expansions[j].ID })
	sort.Slice(diagnostics, func(i, j int) bool { return digest(diagnostics[i]) < digest(diagnostics[j]) })
}

func (s *Service) buildPath(ctx context.Context, se *session, path expander.DiscoveredPath,
	shouldBuild func(expander.DiscoveredPath) bool,
) (*render.Render, error) {
	base := path.BaseDir
	if base == "" {
		base = se.options.Root
	}
	full := filepath.Join(base, path.Path)
	fs := filesys.MakeFsOnDisk()
	if se.options.LocalOnly {
		if err := render.ValidateLocalPath(fs, se.options.Root, base); err != nil {
			return nil, err
		}
		if err := render.ValidateLocalPath(fs, base, full); err != nil {
			return nil, err
		}
	}
	if !fs.Exists(full) {
		return nil, fmt.Errorf("%s: path %q does not exist", path.Producer, full)
	}
	built := build.New(s.log)
	if se.options.LocalOnly {
		built.SetLocalOnly(base)
	}
	visit := func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		build := path
		build.BaseDir, build.Path = dir, "."
		if !shouldBuild(build) {
			return nil
		}
		return built.AddPathWithProducer(fs, dir, path.Producer)
	}
	if se.options.Recursive {
		if err := build.WalkPaths(fs, full, visit); err != nil {
			return nil, err
		}
	} else if err := visit(full); err != nil {
		return nil, err
	}
	if err := built.ApplySubstitutionsToNew(0, path.Producer, path.Substitutions); err != nil {
		return nil, err
	}
	if path.Namespace != "" {
		if err := built.ApplyNamespaceToNew(0, path.Namespace); err != nil {
			return nil, err
		}
	}
	built.MarkProvenanceToNew(0, path.Producer)
	return built.Render, nil
}

func pathKey(root string, path expander.DiscoveredPath) string {
	base := path.BaseDir
	if base == "" {
		base = root
	}
	path.Path = filepath.Join(base, path.Path)
	path.BaseDir, path.Producer = "", ""
	if len(path.Substitutions) == 0 {
		path.Substitutions = nil
	}
	data, _ := json.Marshal(path)
	return string(data)
}

func outputResources(r resmap.ResMap, provenance plugin.Provenance) ([]plugin.Resource, error) {
	result := []plugin.Resource{}
	for _, res := range r.Resources() {
		parsed, err := plugin.ParseResource([]byte(res.MustYaml()), provenance)
		if err != nil {
			return nil, err
		}
		result = append(result, parsed)
	}
	return result, nil
}

func diagnostic(code string, err error, id string) plugin.Diagnostic {
	return plugin.Diagnostic{Code: code, Message: err.Error(), Severity: "error", ResourceID: id}
}

func issues(result *expander.ExpandResult, id string) []plugin.Diagnostic {
	diagnostics := []plugin.Diagnostic{}
	for _, err := range result.Errors {
		diagnostics = append(diagnostics, diagnostic("render-error", err, id))
	}
	for _, err := range result.DeferredErrors {
		diagnostics = append(diagnostics, diagnostic("pending-input", err, id))
	}
	return diagnostics
}

func warnings(r *render.Render) []plugin.Diagnostic {
	diagnostics := []plugin.Diagnostic{}
	for _, err := range r.Warnings() {
		diagnostics = append(diagnostics, diagnostic("duplicate-resource", err, ""))
	}
	return diagnostics
}

func collisions(foreign []plugin.Resource, expansions []plugin.Expansion) []plugin.Diagnostic {
	owners := map[string]string{}
	for _, input := range foreign {
		owners[input.ID] = "foreign inventory"
	}
	diagnostics := []plugin.Diagnostic{}
	for _, expansion := range expansions {
		for _, resource := range expansion.Resources {
			if owner, ok := owners[resource.ID]; ok {
				err := fmt.Errorf("duplicate resource %s produced by %s and %s", resource.ID, owner, expansion.ID)
				diagnostics = append(diagnostics, diagnostic("duplicate-resource", err, resource.ID))
			}
			owners[resource.ID] = expansion.ID
		}
	}
	return diagnostics
}
