package diffsource

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractTarRejectsEscapes(t *testing.T) {
	for _, header := range []tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "/absolute", Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escape"},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/absolute"},
		{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"},
	} {
		t.Run(header.Name+header.Linkname, func(t *testing.T) {
			var data bytes.Buffer
			tw := tar.NewWriter(&data)
			if err := tw.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractTar(t.TempDir(), &data); err == nil {
				t.Fatalf("extractTar(%#v) = nil, want rejection", header)
			}
		})
	}
}

func TestExtractTarConfinesExistingSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	tw := tar.NewWriter(&data)
	if err := tw.WriteHeader(&tar.Header{Name: "escape/file", Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(root, &data); err == nil {
		t.Fatal("extractTar(existing symlink escape) = nil, want rejection")
	}
	if _, err := os.Stat(filepath.Join(outside, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file stat = %v, want not exist", err)
	}
}

func TestMaterializeCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, kind := range []Kind{KindPath, KindWorktree, KindRevision} {
		_, _, err := (Source{Kind: kind, Raw: "."}).Materialize(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Materialize(%s, canceled) = %v, want cancellation", kind, err)
		}
	}
}

func TestExtractTarValidatesResolvedSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []tar.Header
		wantErr bool
	}{
		{name: "chained escape", wantErr: true, headers: []tar.Header{
			{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "."},
			{Name: "b.yaml", Typeflag: tar.TypeSymlink, Linkname: "a/../outside/secret.yaml"},
		}},
		{name: "forward chained escape", wantErr: true, headers: []tar.Header{
			{Name: "b.yaml", Typeflag: tar.TypeSymlink, Linkname: "a/../outside/secret.yaml"},
			{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "."},
		}},
		{name: "safe forward chain", headers: []tar.Header{
			{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "b"},
			{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "dir/file"},
			{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0o600},
		}},
		{name: "dangling link", wantErr: true, headers: []tar.Header{
			{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "missing"},
		}},
		{name: "cyclic links", wantErr: true, headers: []tar.Header{
			{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "b"},
			{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "a"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			tw := tar.NewWriter(&data)
			for _, hdr := range tc.headers {
				if err := tw.WriteHeader(&hdr); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractTar(t.TempDir(), &data); (err != nil) != tc.wantErr {
				t.Fatalf("extractTar(%s) = %v, want error=%v", tc.name, err, tc.wantErr)
			}
		})
	}
}
