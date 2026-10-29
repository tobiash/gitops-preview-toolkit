package fluxrender

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

const resourceSetInput = `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: apps
  namespace: flux-system
spec:
  inputs:
    - id: one
      message: first
  resources:
    - apiVersion: v1
      kind: ConfigMap
      metadata:
        name: 'app-<< inputs.id >>'
        namespace: '<< inputs.provider.namespace >>'
      data:
        message: '<< inputs.message >>'
`

func TestResourceSetInlineExpansionAndRetraction(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, resourceSetInput)
	first := expandSession(t, s, id, control)
	requireClean(t, first)
	if len(first.Expansions) != 1 || first.Expansions[0].Trigger != control.ID || len(outputs(first)) != 1 {
		t.Fatalf("Expand(ResourceSet inline inputs) = %+v, want one triggered expansion and output", first)
	}
	out := outputs(first)[0]
	if out.ID != "v1/ConfigMap/flux-system/app-one" || out.Provenance.Kind != "ResourceSet" || out.Provenance.Name != "apps" || out.Provenance.Namespace != "flux-system" || !strings.Contains(out.YAML, "message: first") {
		t.Fatalf("Expand(ResourceSet inline inputs) output = %+v, want app-one with first message and ResourceSet provenance", out)
	}
	changed := input(t, strings.ReplaceAll(strings.ReplaceAll(resourceSetInput, "id: one", "id: two"), "message: first", "message: second"))
	second := expandSession(t, s, id, append([]plugin.Resource{changed}, outputs(first)...)...)
	requireClean(t, second)
	if len(outputs(second)) != 1 || outputs(second)[0].ID != "v1/ConfigMap/flux-system/app-two" || !strings.Contains(outputs(second)[0].YAML, "message: second") {
		t.Fatalf("Expand(changed ResourceSet) = %+v, want replacement app-two", second)
	}
	sameIdentity := input(t, strings.ReplaceAll(changed.YAML, "message: second", "message: third"))
	third := expandSession(t, s, id, append([]plugin.Resource{sameIdentity}, outputs(second)...)...)
	requireClean(t, third)
	if len(outputs(third)) != 1 || !strings.Contains(outputs(third)[0].YAML, "message: third") {
		t.Errorf("Expand(changed input value with stable identity) = %+v, want app-two with third message", third)
	}
	removed := expandSession(t, s, id, outputs(second)...)
	requireClean(t, removed)
	if len(removed.Expansions) != 0 {
		t.Errorf("Expand(deleted ResourceSet) = %+v, want no expansions", removed)
	}
}

func TestResourceSetTemplateActivatesFluxAndHelm(t *testing.T) {
	s := newService(t)
	root := t.TempDir()
	chartFixture(t, root, "from-resourceset")
	writeFixture(t, root, "child/release.yaml", helmInput)
	id := openSession(t, s, root, nil, false)
	control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: bootstrap
  namespace: flux-system
spec:
  inputs:
    - path: child
  resourcesTemplate: |
    apiVersion: source.toolkit.fluxcd.io/v1
    kind: GitRepository
    metadata:
      name: source
      namespace: flux-system
    spec:
      url: ./
    ---
    apiVersion: kustomize.toolkit.fluxcd.io/v1
    kind: Kustomization
    metadata:
      name: child
      namespace: flux-system
    spec:
      path: << inputs.path | quote >>
`)
	r := expandSession(t, s, id, control)
	requireClean(t, r)
	if len(r.Expansions) != 3 || len(outputs(r)) != 4 {
		t.Fatalf("Expand(ResourceSet -> KS -> HR) = %+v, want three expansions and four outputs", r)
	}
	leaf := false
	for _, out := range outputs(r) {
		if out.Provenance.Kind == "HelmRelease" && strings.Contains(out.YAML, "message: from-resourceset") {
			leaf = true
		}
	}
	if !leaf {
		t.Errorf("Expand(ResourceSet -> KS -> HR) = %+v, want Helm output with HelmRelease provenance", r)
	}
	retracted := expandSession(t, s, id, outputs(r)...)
	requireClean(t, retracted)
	if len(retracted.Expansions) != 0 {
		t.Errorf("Expand(removed ResourceSet with stale subtree) = %+v, want empty closure", retracted)
	}
}

func TestResourceSetProvidersReevaluateAndSelectSameNamespace(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: apps
  namespace: flux-system
spec:
  inputsFrom:
    - kind: ResourceSetInputProvider
      name: settings
    - selector:
        matchLabels:
          app: example
  resourcesTemplate: |
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: app-<< inputs.id >>
      namespace: << inputs.provider.namespace >>
    data:
      message: << inputs.message | quote >>
      provider: << inputs.provider.name | quote >>
`)
	provider := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: settings
  namespace: flux-system
  uid: abc
  labels:
    app: example
spec:
  type: Static
  defaultValues:
    id: ignored
    message: static
`)
	other := input(t, strings.Replace(provider.YAML, "namespace: flux-system", "namespace: elsewhere", 1))
	r := expandSession(t, s, id, control, provider, other)
	requireClean(t, r)
	if len(outputs(r)) != 1 || outputs(r)[0].ID != "v1/ConfigMap/flux-system/app-38600999" || !strings.Contains(outputs(r)[0].YAML, "message: static") {
		t.Fatalf("Expand(Static provider selected twice and foreign namespace) = %+v, want one output with upstream UID-derived ID 38600999", r)
	}
	dynamic := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: settings
  namespace: flux-system
spec:
  type: GitHubBranch
status:
  exportedInputs:
    - id: branch1
      message: snapshot
`)
	r = expandSession(t, s, id, append([]plugin.Resource{control, dynamic}, outputs(r)...)...)
	requireClean(t, r)
	if len(outputs(r)) != 1 || outputs(r)[0].ID != "v1/ConfigMap/flux-system/app-branch1" || !strings.Contains(outputs(r)[0].YAML, "message: snapshot") {
		t.Fatalf("Expand(changed provider snapshot) = %+v, want replacement snapshot output", r)
	}
	empty := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: settings
  namespace: flux-system
spec:
  type: GitHubBranch
status:
  exportedInputs: []
`)
	r = expandSession(t, s, id, append([]plugin.Resource{control, empty}, outputs(r)...)...)
	requireClean(t, r)
	if len(outputs(r)) != 0 {
		t.Errorf("Expand(empty exported inputs) = %+v, want no output", r)
	}
	missing := expandSession(t, s, id, control)
	if len(outputs(missing)) != 0 || len(missing.Diagnostics) != 1 || missing.Diagnostics[0].ResourceID != control.ID {
		t.Errorf("Expand(missing provider) = %+v, want attributed diagnostic and no output", missing)
	}
}

func TestResourceSetPermuteProviderScopesAndIDs(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: My-Apps
  namespace: flux-system
spec:
  inputStrategy:
    name: Permute
  inputs:
    - id: east
    - id: west
  inputsFrom:
    - name: git-tags
  resourcesTemplate: |
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: app-<< inputs.my_apps.id >>-<< inputs.git_tags.id >>
    data:
      id: << inputs.id | quote >>
      kind: << inputs.my_apps.provider.kind | quote >>
      provider: << inputs.git_tags.provider.name | quote >>
`)
	provider := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: git-tags
  namespace: flux-system
spec:
  type: GitHubTag
status:
  exportedInputs:
    - id: v1
    - id: v2
`)
	r := expandSession(t, s, id, control, provider)
	requireClean(t, r)
	if len(outputs(r)) != 4 {
		t.Fatalf("Expand(Permute 2x2) = %+v, want four scoped permutations", r)
	}
	for _, out := range outputs(r) {
		if !strings.Contains(out.YAML, "kind: ResourceSet") || !strings.Contains(out.YAML, "provider: git-tags") {
			t.Errorf("Expand(Permute) output = %+v, want nested provider metadata", out)
		}
	}
	// Upstream IDs checksum the ordered normalized provider name/index pairs.
	if outputs(r)[0].ID != "v1/ConfigMap//app-east-v1" || !strings.Contains(outputs(r)[0].YAML, `id: "1343620949"`) {
		t.Errorf("Expand(first permutation) = %+v, want app-east-v1 and upstream ID 1343620949", outputs(r)[0])
	}
}

func TestResourceSetRejectsUnsupportedRenderingInputs(t *testing.T) {
	for _, extra := range []string{
		"  steps: []\n", "  patches: []\n", "  inputsFrom:\n    - name: settings\n      namespace: other\n",
		"  inputStrategy:\n    name: Unknown\n", "  commonMetadata:\n    annotations:\n      fluxcd.controlplane.io/copyFrom: source\n",
	} {
		t.Run(extra, func(t *testing.T) {
			s := newService(t)
			id := openSession(t, s, t.TempDir(), nil, false)
			control := input(t, resourceSetInput+extra)
			r := expandSession(t, s, id, control)
			if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "render-error" || r.Diagnostics[0].ResourceID != control.ID {
				t.Errorf("Expand(unsupported %q) = %+v, want one attributed render-error and no partial output", extra, r)
			}
		})
	}
}

func TestResourceSetMetadataDeduplicationAndDisabledOutputs(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: apps
  namespace: flux-system
spec:
  commonMetadata:
    labels:
      environment: preview
    annotations:
      note: common
  resources:
    - apiVersion: v1
      kind: ConfigMap
      metadata:
        name: shared
        annotations:
          note: original
      data:
        message: resources-win
  resourcesTemplate: |
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: shared
    data:
      message: ignored-duplicate
    ---
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: disabled
      annotations:
        fluxcd.controlplane.io/reconcile: disabled
    ---
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: templated
    data:
      config: |
        << dict "value" "hello" | toYaml | nindent 4 >>
`)
	r := expandSession(t, s, id, control)
	requireClean(t, r)
	if len(outputs(r)) != 2 {
		t.Fatalf("Expand(metadata, duplicates, disabled outputs) = %+v, want shared and templated", r)
	}
	for _, out := range outputs(r) {
		obj, err := plugin.Object(out)
		if err != nil {
			t.Fatal(err)
		}
		metadata := obj["metadata"].(map[string]any)
		labels := metadata["labels"].(map[string]any)
		annotations := metadata["annotations"].(map[string]any)
		if labels["environment"] != "preview" || labels["resourceset.fluxcd.controlplane.io/name"] != "apps" || annotations["note"] != "common" {
			t.Errorf("Expand(ResourceSet common metadata) = %+v, want common metadata and owner labels", out)
		}
		if out.ID == "v1/ConfigMap//shared" && !strings.Contains(out.YAML, "resources-win") {
			t.Errorf("Expand(duplicate resourcesTemplate identity) = %+v, want resources precedence", out)
		}
	}
}

func TestResourceSetBoundedRenderingIsAtomic(t *testing.T) {
	for _, spec := range []string{
		"  inputs:\n" + strings.Repeat("    - id: x\n", 10001) + "  resources: []\n",
		`  resourcesTemplate: |
    << range $i, $v := inputs.items >>
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: item-<< $i >>
    ---
    << end >>
  inputs:
    - items: [` + strings.Repeat("0,", 10000) + "0]\n",
		"  resourcesTemplate: '<< repeat 1000000000 \"x\" >>'\n",
		"  inputs:\n    - payload: " + strings.Repeat("x", 20000) + "\n      items: [" + strings.Repeat("0,", 999) + `0]
  resourcesTemplate: |
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: oversized
    data:
      message: << range inputs.items >><< inputs.payload >><< end >>
`,
	} {
		t.Run(spec[:min(50, len(spec))], func(t *testing.T) {
			s := newService(t)
			id := openSession(t, s, t.TempDir(), nil, false)
			control := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSet\nmetadata:\n  name: bounded\nspec:\n"+spec)
			r := expandSession(t, s, id, control)
			if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 || r.Diagnostics[0].ResourceID != control.ID {
				t.Errorf("Expand(oversized inputs/template) = %+v, want attributed error and no partial output", r)
			}
		})
	}
}

func TestResourceSetCancelDoesNotPublishOutputs(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Expand(ctx, &plugin.ExpandRequest{Session: id, Resources: []plugin.Resource{input(t, resourceSetInput)}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Expand(canceled ResourceSet) error = %v, want context.Canceled", err)
	}
	r := expandSession(t, s, id, input(t, resourceSetInput))
	requireClean(t, r)
	if len(outputs(r)) != 1 {
		t.Errorf("Expand(after canceled ResourceSet) = %+v, want one successful output", r)
	}
}

func TestResourceSetRejectsNestedControllerTransforms(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: unsupported-transform
spec:
  resources:
    - apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: example
      spec:
        template:
          metadata:
            annotations:
              fluxcd.controlplane.io/checksumFrom: ConfigMap/default/settings
`)
	r := expandSession(t, s, id, control)
	if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 || r.Diagnostics[0].ResourceID != control.ID {
		t.Errorf("Expand(nested checksumFrom) = %+v, want attributed unsupported-transform error", r)
	}
}

func TestResourceSetRejectsUnsupportedStaticProviderFields(t *testing.T) {
	for _, fields := range []string{"  url: https://example.invalid\n", "  defaultValues: null\n", "  schedule: []\n", "  unknown: value\n"} {
		t.Run(fields, func(t *testing.T) {
			s := newService(t)
			id := openSession(t, s, t.TempDir(), nil, false)
			control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: apps
spec:
  inputsFrom:
    - name: settings
  resources:
    - apiVersion: v1
      kind: ConfigMap
      metadata:
        name: example
`)
			provider := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSetInputProvider\nmetadata:\n  name: settings\nspec:\n  type: Static\n"+fields)
			r := expandSession(t, s, id, control, provider)
			if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 {
				t.Errorf("Expand(Static provider with %q) = %+v, want explicit error and no output", fields, r)
			}
		})
	}
}

func TestResourceSetBoundsOutputFreeTemplateExecution(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: recursive
spec:
  resourcesTemplate: |
    <<- define "again" ->><< template "again" >><<- end ->>
    << template "again" >>
`)
	r := expandSession(t, s, id, control)
	if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Message, "operation limit") {
		t.Errorf("Expand(output-free recursion) = %+v, want bounded template operation error", r)
	}
}

func TestResourceSetEmptyInputStrategies(t *testing.T) {
	for _, tt := range []struct {
		name          string
		configuration string
		want          int
	}{
		{name: "no-inputs-renders-once", configuration: "", want: 1},
		{name: "empty-provider-retracts", configuration: "  inputsFrom:\n    - name: empty\n", want: 0},
		{name: "flatten-keeps-inline", configuration: "  inputs:\n    - id: one\n  inputsFrom:\n    - name: empty\n", want: 1},
		{name: "permute-skips-empty", configuration: "  inputStrategy:\n    name: Permute\n  inputs:\n    - id: one\n  inputsFrom:\n    - name: empty\n", want: 1},
		{name: "permute-includes-empty", configuration: "  inputStrategy:\n    name: Permute\n    includeEmptyProviders: true\n  inputs:\n    - id: one\n  inputsFrom:\n    - name: empty\n", want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newService(t)
			id := openSession(t, s, t.TempDir(), nil, false)
			control := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSet\nmetadata:\n  name: apps\nspec:\n"+tt.configuration+"  resources:\n    - apiVersion: v1\n      kind: ConfigMap\n      metadata:\n        name: example\n")
			provider := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSetInputProvider\nmetadata:\n  name: empty\nspec:\n  type: GitHubBranch\nstatus:\n  exportedInputs: []\n")
			r := expandSession(t, s, id, control, provider)
			requireClean(t, r)
			if len(outputs(r)) != tt.want {
				t.Errorf("Expand(%s) output count = %d, want %d", tt.name, len(outputs(r)), tt.want)
			}
		})
	}
}

func TestResourceSetDynamicDiscoveryRequiresSnapshot(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSet\nmetadata:\n  name: apps\nspec:\n  inputsFrom:\n    - name: live\n  resources: []\n")
	provider := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSetInputProvider\nmetadata:\n  name: live\nspec:\n  type: GitHubBranch\n  url: https://example.invalid\n")
	r := expandSession(t, s, id, control, provider)
	if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Message, "status.exportedInputs") {
		t.Errorf("Expand(live dynamic provider without snapshot) = %+v, want explicit exportedInputs requirement", r)
	}
}

func TestResourceSetPermutationLimit(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSet\nmetadata:\n  name: apps\nspec:\n  inputStrategy:\n    name: Permute\n  inputs:\n"+strings.Repeat("    - id: inline\n", 101)+"  inputsFrom:\n    - name: branches\n  resources: []\n")
	provider := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSetInputProvider\nmetadata:\n  name: branches\nspec:\n  type: GitHubBranch\nstatus:\n  exportedInputs:\n"+strings.Repeat("    - id: branch\n", 100))
	r := expandSession(t, s, id, control, provider)
	if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Message, "10000 permutations") {
		t.Errorf("Expand(101x100 input permutations) = %+v, want explicit permutation limit before rendering", r)
	}
}

func TestResourceSetDisabledControlDoesNotPretendOutputsWereDeleted(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, strings.Replace(resourceSetInput, "  namespace: flux-system\n", "  namespace: flux-system\n  annotations:\n    fluxcd.controlplane.io/reconcile: disabled\n", 1))
	r := expandSession(t, s, id, control)
	if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 || r.Diagnostics[0].ResourceID != control.ID {
		t.Errorf("Expand(disabled ResourceSet) = %+v, want explicit live-state limitation instead of clean empty inventory", r)
	}
}

func TestResourceSetBoundsIntermediateFunctionValues(t *testing.T) {
	// Assignments do not write output. Just 32 iterations would materialize
	// gigabytes without the intermediate function-value checks.
	for _, expression := range []string{`cat $v $v`, `printf "%s%s" $v $v`, `print $v $v`, `println $v $v`, `html $v $v`, `js $v $v`, `urlquery $v $v`, `toJson $v`, `quote $v`, `list $v $v`, `concat (list $v) (list $v)`, `append (list $v) $v`} {
		t.Run(expression, func(t *testing.T) {
			s := newService(t)
			id := openSession(t, s, t.TempDir(), nil, false)
			control := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSet\nmetadata:\n  name: intermediate\nspec:\n  inputs:\n    - items: ["+strings.Repeat("0,", 31)+"0]\n  resourcesTemplate: |\n    << $v := \"xxxxxxxxxxxxxxxx\" >>\n    << range inputs.items >><< $v = "+expression+" >><< end >>\n")
			r := expandSession(t, s, id, control)
			if len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Message, "materialization limit") {
				t.Errorf("Expand(intermediate %s) = %+v, want materialization-limit diagnostic before writing output", expression, r)
			}
		})
	}
}

func TestResourceSetRejectsAmplifyingFunctionFormats(t *testing.T) {
	for _, tt := range []struct{ expression, want string }{
		{expression: `join "x" (list "a" "b")`, want: `function "join" not defined`},
		{expression: `replace "x" "xx" "x"`, want: `function "replace" not defined`},
		{expression: `toPrettyJson (dict "x" "y")`, want: `function "toPrettyJson" not defined`},
		{expression: `mustToPrettyJson (dict "x" "y")`, want: `function "mustToPrettyJson" not defined`},
		{expression: `printf "%1000000s%1000000s" "x" "y"`, want: "supports only bare format verbs"},
	} {
		t.Run(tt.expression, func(t *testing.T) {
			s := newService(t)
			id := openSession(t, s, t.TempDir(), nil, false)
			control := input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSet\nmetadata:\n  name: functions\nspec:\n  resourcesTemplate: |\n    << "+tt.expression+" >>\n")
			r := expandSession(t, s, id, control)
			if len(r.Diagnostics) != 1 || len(outputs(r)) != 0 || !strings.Contains(r.Diagnostics[0].Message, tt.want) {
				t.Errorf("Expand(unsupported amplifying expression %s) = %+v, want error containing %q", tt.expression, r, tt.want)
			}
		})
	}
}

func TestResourceSetDeduplicatesAcrossAPIVersions(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: versions
spec:
  inputs:
    - id: one
  resources:
    - apiVersion: example.org/v1
      kind: Example
      metadata:
        name: shared
      spec:
        value: first
    - apiVersion: example.org/v2
      kind: Example
      metadata:
        name: shared
      spec:
        value: second
  resourcesTemplate: |
    apiVersion: example.org/v3
    kind: Example
    metadata:
      name: shared
    spec:
      value: template
`)
	r := expandSession(t, s, id, control)
	requireClean(t, r)
	if len(outputs(r)) != 1 || outputs(r)[0].ID != "example.org/v1/Example//shared" || !strings.Contains(outputs(r)[0].YAML, "value: first") {
		t.Errorf("Expand(cross-version identities) = %+v, want first spec.resources version and value only", r)
	}
}

func TestResourceSetNoInputObjectExclusionDiffersFromTemplate(t *testing.T) {
	for _, tt := range []struct {
		name   string
		inputs string
		want   int
	}{
		{name: "no-input-object-is-retained", want: 1},
		{name: "input-object-is-excluded", inputs: "  inputs:\n    - {}\n", want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newService(t)
			id := openSession(t, s, t.TempDir(), nil, false)
			control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: exclusion
spec:
`+tt.inputs+`  resources:
    - apiVersion: v1
      kind: ConfigMap
      metadata:
        name: object
        annotations:
          fluxcd.controlplane.io/reconcile: disabled
  resourcesTemplate: |
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: template
      annotations:
        fluxcd.controlplane.io/reconcile: disabled
`)
			r := expandSession(t, s, id, control)
			requireClean(t, r)
			if len(outputs(r)) != tt.want {
				t.Fatalf("Expand(%s) output count = %d, want %d", tt.name, len(outputs(r)), tt.want)
			}
			if tt.want == 1 && outputs(r)[0].ID != "v1/ConfigMap//object" {
				t.Errorf("Expand(%s) = %+v, want object-form disabled resource only", tt.name, r)
			}
		})
	}
}

func TestResourceSetPreservesIntegerPrecisionAndScalarTypes(t *testing.T) {
	for _, tt := range []struct {
		name          string
		configuration string
		provider      string
		want          int
	}{
		{name: "inline", configuration: "  inputs:\n    - number: 9007199254740993\n    - number: 9007199254740992\n", want: 2},
		{name: "static", configuration: "  inputsFrom:\n    - name: values\n", provider: "spec:\n  type: Static\n  defaultValues:\n    number: 9007199254740993\n", want: 1},
		{name: "exported", configuration: "  inputsFrom:\n    - name: values\n", provider: "spec:\n  type: GitHubBranch\nstatus:\n  exportedInputs:\n    - number: 9007199254740993\n    - number: 9007199254740992\n", want: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newService(t)
			id := openSession(t, s, t.TempDir(), nil, false)
			control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: precision
spec:
`+tt.configuration+`  resources:
    - apiVersion: example.org/v1
      kind: Example
      metadata:
        name: item-<< inputs.number >>
      spec:
        value: << inputs.number >>
        literal: 9007199254740993
        adjacent: 9007199254740992
        enabled: true
        fraction: 1.25
        text: "9007199254740993"
`)
			inputs := []plugin.Resource{control}
			if tt.provider != "" {
				inputs = append(inputs, input(t, "apiVersion: fluxcd.controlplane.io/v1\nkind: ResourceSetInputProvider\nmetadata:\n  name: values\n"+tt.provider))
			}
			r := expandSession(t, s, id, inputs...)
			requireClean(t, r)
			if len(outputs(r)) != tt.want {
				t.Fatalf("Expand(%s precise inputs) output count = %d, want %d: %+v", tt.name, len(outputs(r)), tt.want, r)
			}
			foundLarge := false
			for _, out := range outputs(r) {
				object, err := plugin.Object(out)
				if err != nil {
					t.Fatal(err)
				}
				spec := object["spec"].(map[string]any)
				if spec["literal"] != int64(9007199254740993) || spec["adjacent"] != int64(9007199254740992) || spec["enabled"] != true || spec["fraction"] != 1.25 || spec["text"] != "9007199254740993" {
					t.Errorf("Expand(%s literal scalar values) = %#v, want exact int64, bool, float64 and string values", tt.name, spec)
				}
				if out.ID == "example.org/v1/Example//item-9007199254740993" && spec["value"] == int64(9007199254740993) {
					foundLarge = true
				}
			}
			if !foundLarge {
				t.Errorf("Expand(%s) = %+v, want exact 9007199254740993 input identity and desired value", tt.name, r)
			}
		})
	}
}

func TestResourceSetRenderedYAMLUsesKubernetesScalarSemantics(t *testing.T) {
	s := newService(t)
	id := openSession(t, s, t.TempDir(), nil, false)
	control := input(t, `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: kubernetes-scalars
spec:
  resourcesTemplate: |
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: date
    data:
      date: 2026-10-29
    ---
    apiVersion: example.org/v1
    kind: Example
    metadata:
      name: booleans
    spec:
      enabled: on
      disabled: off
      affirmative: yes
      negative: no
      quoted: "on"
`)
	r := expandSession(t, s, id, control)
	requireClean(t, r)
	if len(outputs(r)) != 2 {
		t.Fatalf("Expand(Kubernetes YAML scalars) output count = %d, want 2", len(outputs(r)))
	}
	for _, out := range outputs(r) {
		object, err := plugin.Object(out)
		if err != nil {
			t.Fatal(err)
		}
		switch out.ID {
		case "v1/ConfigMap//date":
			data := object["data"].(map[string]any)
			if data["date"] != "2026-10-29" {
				t.Errorf("Expand(ConfigMap date) = %#v, want string 2026-10-29", data["date"])
			}
		case "example.org/v1/Example//booleans":
			spec := object["spec"].(map[string]any)
			if spec["enabled"] != true || spec["disabled"] != false || spec["affirmative"] != true || spec["negative"] != false || spec["quoted"] != "on" {
				t.Errorf("Expand(Kubernetes boolean scalars) = %#v, want on/yes true, off/no false and quoted on string", spec)
			}
		default:
			t.Errorf("Expand(Kubernetes YAML scalars) output ID = %q, want date ConfigMap or booleans Example", out.ID)
		}
	}
}
