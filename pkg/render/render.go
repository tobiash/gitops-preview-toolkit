package render

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

const ProducerAnnotation = "fmp.tobiash.github.io/producer"

var postBuildSubstitutionPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// Provenance identifies the Flux rendering source that produced a resource.
type Provenance struct {
	Kind      string
	Name      string
	Namespace string
	Path      string
	Text      string
}

// String returns the stable display form used by reports and policy checks.
func (p Provenance) String() string {
	if p.Text != "" {
		return p.Text
	}
	switch {
	case p.Kind == "HelmRelease" && p.Namespace != "" && p.Name != "":
		return fmt.Sprintf("HelmRelease %s/%s", p.Namespace, p.Name)
	case p.Kind != "" && p.Namespace != "" && p.Name != "":
		return fmt.Sprintf("%s %s/%s", p.Kind, p.Namespace, p.Name)
	case p.Kind != "" && p.Name != "":
		return fmt.Sprintf("%s %s", p.Kind, p.Name)
	case p.Path != "":
		return fmt.Sprintf("path %s", p.Path)
	default:
		return ""
	}
}

// PathProvenance describes resources rendered from an explicit path.
func PathProvenance(path string) Provenance {
	return Provenance{Kind: "Path", Path: path, Text: fmt.Sprintf("path %s", path)}
}

// TextProvenance preserves existing producer strings while centralizing provenance storage.
func TextProvenance(text string) Provenance {
	return Provenance{Text: text}
}

// HelmReleaseProvenance describes resources rendered from a Flux HelmRelease.
func HelmReleaseProvenance(namespace, name string) Provenance {
	return Provenance{Kind: "HelmRelease", Namespace: namespace, Name: name}
}

// MatchGVK reports true if the resource's GVK matches the target by group and kind.
// Version is ignored because Flux resources exist in multiple API versions.
func MatchGVK(resGvk, target resid.Gvk) bool {
	return resGvk.Group == target.Group && resGvk.Kind == target.Kind
}

// Render holds a set of rendered Kubernetes YAML resources.
type Render struct {
	resmap.ResMap
	log        logr.Logger
	warnings   []error
	provenance map[string]Provenance
}

// ResourceView is the domain view of a rendered Kubernetes resource plus fmp metadata.
type ResourceView struct {
	ID         resid.ResId
	Kind       string
	Name       string
	Namespace  string
	Provenance Provenance
	Producer   string
	Object     map[string]any
	YAML       string
}

// NewDefaultRender creates a passive collection of rendered resources.
func NewDefaultRender(log logr.Logger) *Render {
	return &Render{
		ResMap:     resmap.New(),
		log:        log,
		provenance: make(map[string]Provenance),
	}
}

func (r *Render) absorbResMap(source, producer string, src resmap.ResMap) error {
	for _, res := range src.Resources() {
		id := res.CurId()
		idKey := id.String()
		newProvenance := provenanceForResource(res, TextProvenance(producer))
		if rendered, ok := src.(*Render); ok {
			if p := rendered.ProvenanceForID(id); p.String() != "" {
				newProvenance = p
			}
		}
		if existing, err := r.GetById(id); err == nil {
			existingProvenance := r.provenance[idKey]
			if existingProvenance.String() == "" {
				existingProvenance = provenanceForResource(existing, TextProvenance("existing resources"))
			}
			_ = r.Remove(existing.CurId())
			r.warnings = append(r.warnings, duplicateWarning(id.String(), existingProvenance.String(), newProvenance.String(), source))
			r.log.V(1).Info("replacing duplicate resource", "id", id)
		}
		if err := r.Append(res); err != nil {
			return err
		}
		r.provenance[idKey] = newProvenance
	}
	return nil
}

// Warnings returns non-fatal issues encountered while building the resource set.
func (r *Render) Warnings() []error {
	return append([]error(nil), r.warnings...)
}

// AbsorbAll merges resources into the render, replacing duplicates and recording warnings.
func (r *Render) AbsorbAll(src resmap.ResMap) error {
	if rendered, ok := src.(*Render); ok {
		r.warnings = append(r.warnings, rendered.Warnings()...)
	}
	return r.absorbResMap("expanded resources", "expanded resources", src)
}

// MarkProvenanceToNew records producer metadata for resources added after count.
func (r *Render) MarkProvenanceToNew(count int, producer string) {
	for _, res := range r.Resources()[count:] {
		r.provenance[res.CurId().String()] = provenanceForResource(res, TextProvenance(producer))
	}
}

// ApplySubstitutionsToNew applies Flux postBuild inline substitutions to
// resources added after count. Missing variables are intentionally preserved so
// the report still shows unresolved substituteFrom or strict-mode inputs.
func (r *Render) ApplySubstitutionsToNew(count int, producer string, substitutions map[string]string) error {
	if len(substitutions) == 0 {
		return nil
	}

	newResources := append([]*resource.Resource(nil), r.Resources()[count:]...)
	for _, res := range newResources {
		delete(r.provenance, res.CurId().String())
		if err := r.Remove(res.CurId()); err != nil {
			return err
		}
	}

	for _, res := range newResources {
		yaml := applyPostBuildSubstitutions(res.MustYaml(), substitutions)
		resources, err := resmap.NewFactory(resource.NewFactory(nil)).NewResMapFromBytes([]byte(yaml))
		if err != nil {
			return fmt.Errorf("applying postBuild substitutions to %s: %w", res.CurId(), err)
		}
		if err := r.absorbResMap("postBuild substitutions", producer, resources); err != nil {
			return err
		}
	}
	return nil
}

func applyPostBuildSubstitutions(input string, substitutions map[string]string) string {
	return postBuildSubstitutionPattern.ReplaceAllStringFunc(input, func(token string) string {
		expr := token[2 : len(token)-1]
		if key, fallback, ok := strings.Cut(expr, ":="); ok {
			if value, exists := substitutions[key]; exists && value != "" {
				return value
			}
			return fallback
		}
		if key, fallback, ok := strings.Cut(expr, ":-"); ok {
			if value, exists := substitutions[key]; exists && value != "" {
				return value
			}
			return fallback
		}
		if value, ok := substitutions[expr]; ok {
			return value
		}
		return token
	})
}

// ProvenanceForID returns the structured provenance for a resource ID.
func (r *Render) ProvenanceForID(id resid.ResId) Provenance {
	return r.provenance[id.String()]
}

// SetProvenance stores exact structured origin metadata without modifying YAML.
func (r *Render) SetProvenance(id resid.ResId, p Provenance) {
	if r.provenance == nil {
		r.provenance = make(map[string]Provenance)
	}
	r.provenance[id.String()] = p
}

// AbsorbWithProducer merges already-built resources with source context for
// duplicate diagnostics. It performs no filesystem reads or rendering.
func (r *Render) AbsorbWithProducer(source, producer string, src resmap.ResMap) error {
	return r.absorbResMap(source, producer, src)
}

// ResourceViewForID returns a rendered resource and its fmp metadata.
func (r *Render) ResourceViewForID(id resid.ResId) (ResourceView, bool) {
	res, _ := r.GetByCurrentId(id)
	if res == nil {
		return ResourceView{}, false
	}
	obj, _ := res.Map()
	provenance := r.ProvenanceForID(id)
	return ResourceView{
		ID:         id,
		Kind:       res.GetKind(),
		Name:       res.GetName(),
		Namespace:  res.GetNamespace(),
		Provenance: provenance,
		Producer:   provenance.String(),
		Object:     obj,
		YAML:       res.MustYaml(),
	}, true
}

func duplicateWarning(id, existingProducer, newProducer, source string) error {
	if existingProducer == "" {
		existingProducer = "existing resources"
	}
	if newProducer == "" {
		newProducer = source
	}
	if existingProducer == newProducer {
		return fmt.Errorf("duplicate resource %s produced by %s replaced an earlier instance from the same producer", id, newProducer)
	}
	return fmt.Errorf("duplicate resource %s produced by %s replaced an existing resource produced by %s", id, newProducer, existingProducer)
}

func provenanceForResource(res *resource.Resource, fallback Provenance) Provenance {
	annotations := res.GetAnnotations()
	producer := annotations[ProducerAnnotation]
	labels := res.GetLabels()
	if name := labels["helm.toolkit.fluxcd.io/name"]; name != "" {
		ns := labels["helm.toolkit.fluxcd.io/namespace"]
		origin := HelmReleaseProvenance(ns, name)
		// Helm's runner writes matching source labels and a display annotation.
		// Unrelated explicit producer annotations must still take precedence.
		if ns != "" && producer == origin.String() {
			return origin
		}
		if ns == "" {
			ns = res.GetNamespace()
		}
		if producer == "" {
			return HelmReleaseProvenance(ns, name)
		}
	}
	if producer != "" {
		return TextProvenance(producer)
	}
	return fallback
}

// Sort orders resources by (kind, namespace, name) for deterministic output.
// This is critical for diff stability across runs.
func (r *Render) Sort() {
	resources := r.Resources()
	sort.Slice(resources, func(i, j int) bool {
		a, b := resources[i], resources[j]
		if a.GetKind() != b.GetKind() {
			return a.GetKind() < b.GetKind()
		}
		if a.GetNamespace() != b.GetNamespace() {
			return a.GetNamespace() < b.GetNamespace()
		}
		return a.GetName() < b.GetName()
	})
	r.Clear()
	for _, res := range resources {
		_ = r.Append(res)
	}
}

// FilterByLabel removes all resources that do not have the given label key
// set to the given value. Resources without the label are removed.
func (r *Render) FilterByLabel(key, value string) {
	for _, res := range r.Resources() {
		if res.GetLabels()[key] != value {
			_ = r.Remove(res.CurId())
		}
	}
}

// FilterCRDs removes all CustomResourceDefinition resources from the render.
func (r *Render) FilterCRDs() {
	for _, res := range r.Resources() {
		if res.GetKind() == "CustomResourceDefinition" {
			_ = r.Remove(res.CurId())
		}
	}
}

// ApplyNamespaceToNew sets the namespace on all namespace-scoped resources
// added after the given count. This is used to apply Flux Kustomization
// spec.targetNamespace to newly rendered resources.
func (r *Render) ApplyNamespaceToNew(count int, namespace string) error {
	clusterScoped := make(map[string]bool)
	for _, res := range r.Resources() {
		if res.GetKind() != "CustomResourceDefinition" || res.GetGvk().Group != "apiextensions.k8s.io" {
			continue
		}
		scope, _ := res.GetFieldValue("spec.scope")
		group, _ := res.GetFieldValue("spec.group")
		kind, _ := res.GetFieldValue("spec.names.kind")
		if scope == "Cluster" {
			clusterScoped[fmt.Sprint(group)+"/"+fmt.Sprint(kind)] = true
		}
	}
	resources := append([]*resource.Resource(nil), r.Resources()[count:]...)
	producers := make([]Provenance, len(resources))
	for i, res := range resources {
		producers[i] = r.ProvenanceForID(res.CurId())
		delete(r.provenance, res.CurId().String())
		if err := r.Remove(res.CurId()); err != nil {
			return err
		}
	}
	for i, res := range resources {
		gvk := res.GetGvk()
		if !gvk.IsClusterScoped() && !clusterScoped[gvk.Group+"/"+gvk.Kind] && (gvk.Group != "snapshot.storage.k8s.io" || gvk.Kind != "VolumeSnapshotClass") {
			if err := res.SetNamespace(namespace); err != nil {
				return err
			}
		}
		one := resmap.New()
		if err := one.Append(res); err != nil {
			return err
		}
		if err := r.absorbResMap("targetNamespace", producers[i].String(), one); err != nil {
			return err
		}
		if producers[i] != (Provenance{}) {
			r.SetProvenance(res.CurId(), producers[i])
		}
	}
	return nil
}

// AsJSON returns the rendered resources as a Kubernetes List envelope.
func (r *Render) AsJSON() ([]byte, error) {
	resources := r.Resources()
	items := make([]map[string]any, 0, len(resources))
	for i, res := range resources {
		m, err := res.Map()
		if err != nil {
			return nil, fmt.Errorf("converting resource %d to JSON map: %w", i+1, err)
		}
		items = append(items, m)
	}
	list := map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"items":      items,
	}
	return json.MarshalIndent(list, "", "  ")
}
