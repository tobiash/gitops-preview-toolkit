package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

func TestCacheRetainsOnlyBoundedArtifacts(t *testing.T) {
	root := t.TempDir()
	var source strings.Builder
	source.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: many-keys\ndata:\n")
	for i := range 10000 {
		fmt.Fprintf(&source, "  k%05d: x\n", i)
	}
	writeFixture(t, root, "manifests/config.yaml", source.String())
	s := newTestService(t, root, Options{MaxBytes: 512 << 10})
	id := renderID(t, s)
	// This type-level assertion catches reintroduction of retained maps/ASTs,
	// independently of GC timing or unrelated process heap allocations.
	typ := reflect.TypeFor[cachedEntry]()
	if typ.NumField() != 2 || typ.Field(0).Type != reflect.TypeFor[time.Time]() || typ.Field(1).Type != reflect.TypeFor[[]byte]() {
		t.Fatalf("cachedEntry retains unexpected state: %v", typ)
	}
	cached := s.entries[id]
	artifactBytes := len(cached.data)
	if int64(len(cached.data)) != s.bytes || len(cached.data) != cap(cached.data) || s.bytes > s.opts.MaxBytes {
		t.Fatalf("cache bytes=%d len=%d cap=%d limit=%d", s.bytes, len(cached.data), cap(cached.data), s.opts.MaxBytes)
	}
	// Keep the independent review's heap measurement informational: allocator
	// classes and transient library pools are outside the artifact byte budget.
	runtime.GC()
	var retained, released runtime.MemStats
	runtime.ReadMemStats(&retained)
	cached = nil
	execute(t, s, Request{Operation: "release", ID: id})
	runtime.GC()
	runtime.ReadMemStats(&released)
	t.Logf("MaxBytes=%d, artifact bytes=%d, released heap=%d, source=%d", s.opts.MaxBytes, artifactBytes, int64(retained.HeapAlloc)-int64(released.HeapAlloc), source.Len())
	if s.bytes != 0 || len(s.entries) != 0 {
		t.Fatalf("release retained bytes=%d handles=%d", s.bytes, len(s.entries))
	}
	runtime.KeepAlive(s)
}

func TestNamespaceDeletionPolicyInput(t *testing.T) {
	changes := &diff.DiffResult{Deleted: []diff.ResourceChange{{Kind: "Namespace", Name: "removed", Action: "deleted"}}}
	result, err := policy.Evaluate(t.Context(), changes, &config.PolicyConfig{Builtin: []string{"namespace_delete"}, FailOn: []string{"namespace_delete"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.PolicyFailed || len(result.Classifications) != 1 || result.Classifications[0].Namespace != "" {
		t.Fatalf("Namespace deletion policy = %+v, want failure with an empty-namespace classification", result)
	}
}

func TestCacheCapacityIsExactAndAtomic(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "manifests/config.yaml", configMap)
	s := newTestService(t, root, Options{})
	id := renderID(t, s)
	used := s.bytes
	e, err := s.entries[id].decode(id, true)
	if err != nil {
		t.Fatal(err)
	}
	s.opts.MaxBytes = used*2 - 1
	if err := s.store(e); err != errCapacity {
		t.Fatalf("store beyond budget: %v", err)
	}
	if s.bytes != used || len(s.entries) != 1 {
		t.Fatal("failed store changed accounting")
	}
	s.opts.MaxBytes = used * 2
	if err := s.store(e); err != nil {
		t.Fatalf("store at exact budget: %v", err)
	}
	if s.bytes != used*2 {
		t.Fatalf("bytes=%d, want %d", s.bytes, used*2)
	}
	s.opts.MaxBytes = used * 3
	if err := s.store(e, e); err != errCapacity {
		t.Fatalf("multi-store beyond budget: %v", err)
	}
	if s.bytes != used*2 || len(s.entries) != 2 {
		t.Fatal("partial multi-store escaped")
	}
}

func TestSerializedSnapshotsPreserveComparisonAndOrigins(t *testing.T) {
	root := t.TempDir()
	manifest := "# preserved comment\napiVersion: example.io/v1\nkind: Example\nmetadata:\n  name: object\n  annotations:\n    fmp.tobiash.github.io/producer: custom source\nspec:\n  precise: 9007199254740993\n  quoted: '007'\n  explicit: !!str 123\n  nothing: null\n  value: old\n"
	writeFixture(t, root, "manifests/object.yaml", manifest)
	p, err := preview.New(preview.WithLogger(logr.Discard()), preview.WithPaths([]string{"east:manifests", "west:manifests"}, false), preview.WithLocalOnly())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	before, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "manifests/object.yaml", strings.Replace(manifest, "value: old", "value: new", 1))
	after, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := preview.CompareSnapshots(t.Context(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	a, err := snapshotEntry(before, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := snapshotEntry(after, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestService(t, root, Options{})
	if err := s.store(a, b); err != nil {
		t.Fatal(err)
	}
	left, err := s.entries[a.id].decode(a.id, true)
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.entries[b.id].decode(b.id, true)
	if err != nil {
		t.Fatal(err)
	}
	for cluster, r := range before.Clusters {
		for i, resource := range r.Resources() {
			if got := left.snapshot.Clusters[cluster].ProvenanceForID(resource.CurId()); got != r.ProvenanceForID(resource.CurId()) {
				t.Fatalf("named provenance changed on round trip: %+v", got)
			}
			if got := left.snapshot.Clusters[cluster].Resources()[i].MustYaml(); got != resource.MustYaml() {
				t.Fatalf("YAML changed on round trip:\n%s\nwant:\n%s", got, resource.MustYaml())
			}
		}
	}
	got, err := preview.CompareSnapshots(t.Context(), left.snapshot, right.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	restoreOrigins(got, left.records, right.records)
	if !reflect.DeepEqual(got.ToJSON(), want.ToJSON()) {
		t.Fatalf("comparison changed after serialization:\ngot %+v\nwant %+v", got.ToJSON(), want.ToJSON())
	}
	// Remove inputs entirely: the public compare operation must not rerender.
	if err := os.RemoveAll(filepath.Join(root, "manifests")); err != nil {
		t.Fatal(err)
	}
	d := execute(t, s, Request{Operation: "compare", BeforeID: a.id, AfterID: b.id}).Data.(PreviewData)
	if d.Summary.Modified != 2 {
		t.Fatalf("comparison = %+v", d)
	}
	page := execute(t, s, Request{Operation: "query", ID: a.id}).Data.(QueryData)
	inspection := execute(t, s, Request{Operation: "inspect", ID: a.id, ResourceID: page.Items[0].ResourceID, Fields: []string{"/spec/precise"}}).Data.(InspectData)
	if inspection.New["/spec/precise"] != json.Number("9007199254740993") {
		t.Fatalf("integer lost precision: %+v", inspection.New)
	}
}

func TestSOPSRequiredRenderDeniedInBothProfiles(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprint(trusted), func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, ".fmp.yaml", "paths: [manifests]\nsops-decrypt: true\n")
			writeFixture(t, root, "manifests/config.yaml", configMap)
			s := newTestService(t, root, Options{Trusted: trusted})
			execute(t, s, Request{Operation: "discover"})
			expectError(t, s, Request{Operation: "render"}, "PermissionDenied")
			expectError(t, s, Request{Operation: "preview", Base: "worktree"}, "PermissionDenied")
			if len(s.entries) != 0 {
				t.Fatal("denied render published handles")
			}
		})
	}
}

func TestCaptureMetadataTimeoutAndClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper requires Unix")
	}
	for _, closeSession := range []bool{false, true} {
		t.Run(fmt.Sprint(closeSession), func(t *testing.T) {
			root, bin := t.TempDir(), t.TempDir()
			writeFixture(t, root, "manifests/config.yaml", configMap)
			started := filepath.Join(bin, "started")
			// Keep a child holding inherited pipes open after CommandContext
			// kills the shell, exercising the helper's pipe-drain bound too.
			writeFixture(t, bin, "git", "#!/bin/sh\n: > \"$AGENT_TEST_STARTED\"\nsleep 3\n")
			if err := os.Chmod(filepath.Join(bin, "git"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENT_TEST_STARTED", started)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			timeout := 100 * time.Millisecond
			if closeSession {
				timeout = 10 * time.Second
			}
			s := newTestService(t, root, Options{Timeout: timeout})
			result := make(chan Response, 1)
			go func() {
				result <- s.Execute(context.Background(), Request{Operation: "render", Paths: []string{"manifests"}})
			}()
			deadline := time.After(2 * time.Second)
			for {
				if _, err := os.Stat(started); err == nil {
					break
				}
				select {
				case <-deadline:
					t.Fatal("git helper did not start")
				case <-time.After(time.Millisecond):
				}
			}
			start := time.Now()
			if closeSession {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case r := <-result:
				if r.Error == nil || r.Error.Code != "Canceled" {
					t.Fatalf("capture cancellation = %+v", r)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("capture ignored cancellation")
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("cancellation blocked gate for %v", elapsed)
			}
			if !closeSession {
				execute(t, s, Request{Operation: "discover"})
			}
		})
	}
}
