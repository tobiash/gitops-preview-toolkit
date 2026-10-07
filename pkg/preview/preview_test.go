package preview

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
)

func TestTest_Success(t *testing.T) {
	opts := []Opt{
		WithLogger(logr.Discard()),
		WithPaths([]string{"simple"}, false),
	}
	p, err := newTestPreview(t, opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	testdata := filepath.Join("testdata")
	var buf bytes.Buffer
	if err := p.Test(context.Background(), testdata, &buf); err != nil {
		t.Fatalf("Test() error = %v, output: %s", err, buf.String())
	}
	if buf.String() != "PASS\n" {
		t.Errorf("expected 'PASS\n', got %q", buf.String())
	}
}

func TestTest_Failure(t *testing.T) {
	opts := []Opt{
		WithLogger(logr.Discard()),
		WithPaths([]string{"broken"}, false),
	}
	p, err := newTestPreview(t, opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	testdata := filepath.Join("testdata")
	var buf bytes.Buffer
	if err := p.Test(context.Background(), testdata, &buf); err == nil {
		t.Fatal("expected error from Test() with broken kustomization")
	}
	if !bytes.Contains(buf.Bytes(), []byte("FAIL")) {
		t.Errorf("expected output to contain 'FAIL', got %q", buf.String())
	}
}

func TestWithSort(t *testing.T) {
	p, err := New(WithSort())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !p.sortOutput {
		t.Error("expected sortOutput to be true")
	}
}

func TestWithExcludeCRDs(t *testing.T) {
	p, err := New(WithExcludeCRDs())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !p.excludeCRDs {
		t.Error("expected excludeCRDs to be true")
	}
}

func TestWithHelmReleaseFilter(t *testing.T) {
	p, err := New(WithHelmReleaseFilter("my-release"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.helmReleaseName != "my-release" {
		t.Errorf("expected helmReleaseName 'my-release', got %q", p.helmReleaseName)
	}
}
