package diff

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/hasher"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
)

func TestDiff_AddedResource(t *testing.T) {
	a := render.NewDefaultRender(logr.Discard())
	b := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: new-cm
  namespace: default
data:
  key: value
`)

	var buf bytes.Buffer
	if err := Diff(a, b, &buf); err != nil {
		t.Fatalf("Diff() error = %v", err)
	}

	if buf.Len() == 0 {
		t.Error("expected non-empty diff output for added resource")
	}
	if !bytes.Contains(buf.Bytes(), []byte("new-cm")) {
		t.Error("expected diff to mention 'new-cm'")
	}
}

func TestDiff_DeletedResource(t *testing.T) {
	a := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: old-cm
  namespace: default
data:
  key: value
`)
	b := render.NewDefaultRender(logr.Discard())

	var buf bytes.Buffer
	if err := Diff(a, b, &buf); err != nil {
		t.Fatalf("Diff() error = %v", err)
	}

	if buf.Len() == 0 {
		t.Error("expected non-empty diff output for deleted resource")
	}
	if !bytes.Contains(buf.Bytes(), []byte("old-cm")) {
		t.Error("expected diff to mention 'old-cm'")
	}
}

func TestDiff_ModifiedResource(t *testing.T) {
	a := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: default
data:
  key: old-value
`)
	b := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: default
data:
  key: new-value
`)

	var buf bytes.Buffer
	if err := Diff(a, b, &buf); err != nil {
		t.Fatalf("Diff() error = %v", err)
	}

	if buf.Len() == 0 {
		t.Error("expected non-empty diff output for modified resource")
	}
	if !bytes.Contains(buf.Bytes(), []byte("old-value")) {
		t.Error("expected diff to contain 'old-value'")
	}
	if !bytes.Contains(buf.Bytes(), []byte("new-value")) {
		t.Error("expected diff to contain 'new-value'")
	}
}

func TestDiff_IdenticalResources(t *testing.T) {
	yaml := `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: default
data:
  key: same
`
	a := makeRender(t, yaml)
	b := makeRender(t, yaml)

	var buf bytes.Buffer
	if err := Diff(a, b, &buf); err != nil {
		t.Fatalf("Diff() error = %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("expected empty diff for identical resources, got:\n%s", buf.String())
	}
}

func makeRender(t *testing.T, yaml string) *render.Render {
	t.Helper()
	r := render.NewDefaultRender(logr.Discard())
	resFactory := resource.NewFactory(&hasher.Hasher{})
	rmFactory := resmap.NewFactory(resFactory)

	rm, err := rmFactory.NewResMapFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("failed to create resmap: %v", err)
	}
	if err := r.AbsorbAll(rm); err != nil {
		t.Fatalf("failed to absorb: %v", err)
	}
	return r
}

func TestDiffResult_ToJSON(t *testing.T) {
	a := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: default
data:
  key: old-value
`)
	b := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: default
data:
  key: new-value
`)

	var diffBuf bytes.Buffer
	result, err := DiffWithResult(a, b, &diffBuf)
	if err != nil {
		t.Fatalf("DiffWithResult() error = %v", err)
	}

	jsonResult := result.ToJSON()

	// Verify Modified contains the changed resource
	if len(jsonResult.Modified) != 1 {
		t.Fatalf("expected 1 modified resource, got %d", len(jsonResult.Modified))
	}

	change := jsonResult.Modified[0]
	if change.ObjectRef.Kind != "ConfigMap" {
		t.Errorf("expected Kind ConfigMap, got %s", change.ObjectRef.Kind)
	}
	if change.ObjectRef.Name != "my-cm" {
		t.Errorf("expected Name my-cm, got %s", change.ObjectRef.Name)
	}
	if change.ObjectRef.Namespace != "default" {
		t.Errorf("expected Namespace default, got %s", change.ObjectRef.Namespace)
	}
	if change.ObjectRef.APIVersion != "v1" {
		t.Errorf("expected APIVersion v1, got %s", change.ObjectRef.APIVersion)
	}
	if change.UnifiedDiff == "" {
		t.Error("expected non-empty UnifiedDiff")
	}

	// Verify JSON roundtrip
	data, err := json.Marshal(jsonResult)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var roundtrip DiffResultJSON
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(roundtrip.Modified) != 1 {
		t.Fatalf("roundtrip: expected 1 modified, got %d", len(roundtrip.Modified))
	}
}

func TestDiffResult_ToJSON_AddedDeleted(t *testing.T) {
	a := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: old-cm
  namespace: default
`)
	b := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: new-cm
  namespace: default
`)

	var diffBuf bytes.Buffer
	result, err := DiffWithResult(a, b, &diffBuf)
	if err != nil {
		t.Fatalf("DiffWithResult() error = %v", err)
	}

	jsonResult := result.ToJSON()

	if len(jsonResult.Added) != 1 {
		t.Errorf("expected 1 added, got %d", len(jsonResult.Added))
	}
	if len(jsonResult.Deleted) != 1 {
		t.Errorf("expected 1 deleted, got %d", len(jsonResult.Deleted))
	}
	if len(jsonResult.Modified) != 0 {
		t.Errorf("expected 0 modified, got %d", len(jsonResult.Modified))
	}

	meta, _ := jsonResult.Added[0]["metadata"].(map[string]any)
	addedName, _ := meta["name"].(string)
	if addedName != "new-cm" {
		t.Errorf("expected added name new-cm, got %s", addedName)
	}
	if jsonResult.Deleted[0].Name != "old-cm" {
		t.Errorf("expected deleted name old-cm, got %s", jsonResult.Deleted[0].Name)
	}
}

func TestDiff_JSONOutputStructure(t *testing.T) {
	a := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: default
data:
  key: old-value
`)
	b := makeRender(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: default
data:
  key: new-value
`)

	var diffBuf bytes.Buffer
	_, err := DiffWithResult(a, b, &diffBuf)
	if err != nil {
		t.Fatalf("DiffWithResult() error = %v", err)
	}

	// Verify unified diff output is present
	if diffBuf.Len() == 0 {
		t.Error("expected non-empty unified diff output")
	}
	if !bytes.Contains(diffBuf.Bytes(), []byte("old-value")) {
		t.Error("expected diff to contain 'old-value'")
	}
	if !bytes.Contains(diffBuf.Bytes(), []byte("new-value")) {
		t.Error("expected diff to contain 'new-value'")
	}
}
