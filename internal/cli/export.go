package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

const (
	maxExportFiles = 10000
	maxExportBytes = 64 << 20
)

type exportIdentity struct {
	cluster string
	id      string
	logical bool
}

type exportFile struct {
	path string
	data []byte
}

// exportSnapshot writes the exact retained target, never rerendering it. The
// destination must be absent or empty so previous exports cannot leave stale
// resources behind. Writes are confined by os.Root and never follow an existing
// manifest symlink; failures remove only entries created by this invocation.
func exportSnapshot(
	ctx context.Context, snapshot *preview.Snapshot, changes *diff.DiffResult, directory string, changedOnly bool,
) (resultErr error) {
	if directory == "" {
		return fmt.Errorf("export directory is required")
	}
	files, err := exportFiles(ctx, snapshot, changes, changedOnly)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, root.Close())
	}()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(1)
	closeErr := dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) != 0 {
		return fmt.Errorf("export directory must be empty: %s", directory)
	}
	created := []string{}
	defer func() {
		if resultErr == nil {
			return
		}
		for i := len(created) - 1; i >= 0; i-- {
			resultErr = errors.Join(resultErr, root.Remove(created[i]))
		}
	}()
	directories := map[string]bool{}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent := filepath.Dir(file.path)
		if parent != "." && !directories[parent] {
			if err := root.Mkdir(parent, 0o700); err != nil {
				return err
			}
			created = append(created, parent)
			directories[parent] = true
		}
		out, err := root.OpenFile(file.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		created = append(created, file.path)
		_, writeErr := out.Write(file.data)
		if err := errors.Join(writeErr, out.Close()); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func exportFiles(
	ctx context.Context, snapshot *preview.Snapshot, changes *diff.DiffResult, changedOnly bool,
) ([]exportFile, error) {
	if snapshot == nil || !snapshot.Complete || snapshot.Clusters == nil {
		return nil, fmt.Errorf("cannot export an incomplete target snapshot")
	}
	selected := map[exportIdentity]bool{}
	if changedOnly {
		if changes == nil {
			return nil, fmt.Errorf("changed-only export requires a complete comparison")
		}
		for _, group := range [][]diff.ResourceChange{changes.Added, changes.Modified} {
			for _, change := range group {
				key := exportIdentity{cluster: change.Cluster, id: change.ID.String()}
				if change.LogicalID != "" {
					key.id, key.logical = change.LogicalID, true
				}
				selected[key] = true
			}
		}
	}
	clustered := len(snapshot.Clusters) != 1 || snapshot.Clusters[""] == nil
	files := []exportFile{}
	paths := map[string]bool{}
	var total int
	add := func(key exportIdentity, data []byte) error {
		if changedOnly && !selected[key] {
			return nil
		}
		if len(files) >= maxExportFiles || len(data) > maxExportBytes-total {
			return fmt.Errorf("manifest export exceeds %d files or %d bytes", maxExportFiles, maxExportBytes)
		}
		prefix := "resource"
		if key.logical {
			prefix = "logical"
		}
		name := fmt.Sprintf("%s-%x.yaml", prefix, sha256.Sum256([]byte(key.id)))
		if clustered {
			name = filepath.Join(fmt.Sprintf("cluster-%x", sha256.Sum256([]byte(key.cluster))), name)
		}
		if paths[name] {
			return fmt.Errorf("duplicate export identity in cluster %q", key.cluster)
		}
		paths[name] = true
		total += len(data)
		files = append(files, exportFile{path: name, data: data})
		return nil
	}
	for cluster, rendered := range snapshot.Clusters {
		if rendered == nil || rendered.ResMap == nil {
			return nil, fmt.Errorf("cluster %q has no target inventory", cluster)
		}
		for _, resource := range rendered.Resources() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			data, err := resource.AsYAML()
			if err != nil {
				return nil, err
			}
			if err := add(exportIdentity{cluster: cluster, id: resource.CurId().String()}, data); err != nil {
				return nil, err
			}
		}
	}
	for cluster, resources := range snapshot.Logical {
		if _, exists := snapshot.Clusters[cluster]; !exists {
			return nil, fmt.Errorf("logical resources reference unknown cluster %q", cluster)
		}
		if _, err := diff.LogicalChangeSet(resources, resources); err != nil {
			return nil, err
		}
		for _, resource := range resources {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := add(exportIdentity{cluster: cluster, id: resource.ID, logical: true}, []byte(resource.YAML)); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}
