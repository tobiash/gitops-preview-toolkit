package expander

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/resmap"
)

type mockExpander struct {
	result resmap.ResMap
	err    error
	called int
}

func (m *mockExpander) Expand(_ context.Context, r *render.Render) (*ExpandResult, error) {
	m.called++
	return &ExpandResult{Resources: m.result}, m.err
}

func TestRegistry_Expand_NoExpanders(t *testing.T) {
	reg := NewRegistry(logr.Discard())
	r := render.NewDefaultRender(logr.Discard())

	result, err := reg.Expand(context.Background(), r)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if result.Resources.Size() != 0 {
		t.Errorf("expected empty result, got %d resources", result.Resources.Size())
	}
}

func TestRegistry_Expand_SingleExpander(t *testing.T) {
	reg := NewRegistry(logr.Discard())
	mock := &mockExpander{result: resmap.New()}
	reg.Register(mock)

	r := render.NewDefaultRender(logr.Discard())
	_, err := reg.Expand(context.Background(), r)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if mock.called != 1 {
		t.Errorf("expected expander called once, got %d", mock.called)
	}
}

func TestRegistry_Expand_MultipleExpanders(t *testing.T) {
	reg := NewRegistry(logr.Discard())
	m1 := &mockExpander{result: resmap.New()}
	m2 := &mockExpander{result: resmap.New()}
	reg.Register(m1)
	reg.Register(m2)

	r := render.NewDefaultRender(logr.Discard())
	_, err := reg.Expand(context.Background(), r)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if m1.called != 1 || m2.called != 1 {
		t.Errorf("expected each expander called once, got m1=%d m2=%d", m1.called, m2.called)
	}
}

func TestRegistry_Expand_ErrorStops(t *testing.T) {
	reg := NewRegistry(logr.Discard())
	m1 := &mockExpander{err: errors.New("boom")}
	m2 := &mockExpander{}
	reg.Register(m1)
	reg.Register(m2)

	r := render.NewDefaultRender(logr.Discard())
	_, err := reg.Expand(context.Background(), r)
	if err == nil {
		t.Fatal("expected error from failing expander")
	}
	if m2.called != 0 {
		t.Error("expected second expander not called after first failure")
	}
}
