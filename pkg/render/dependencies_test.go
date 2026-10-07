package render_test

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Inspect runtime dependencies, not test imports: renderer integration tests
// legitimately exercise plugin executables while the host must stay engine-free.
func TestHostRuntimeDependenciesExcludeRenderingEngines(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "list", "-deps", "-f", "{{.ImportPath}}", ".", "../preview", "../agent")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		t.Fatalf("listing host runtime dependencies: %v\n%s", err, &stderr)
	}
	for _, dependency := range strings.Fields(string(out)) {
		for _, forbidden := range []string{
			"helm.sh/helm/",
			"github.com/fluxcd/",
			"sigs.k8s.io/kustomize/api/krusty",
			"github.com/tobiash/gitops-preview-toolkit/pkg/build",
			"github.com/tobiash/gitops-preview-toolkit/pkg/fluxrender",
			"github.com/tobiash/gitops-preview-toolkit/pkg/crossplanerender",
			"github.com/tobiash/gitops-preview-toolkit/pkg/expander",
		} {
			if strings.HasPrefix(dependency, forbidden) {
				t.Errorf("host runtime links forbidden rendering dependency %s", dependency)
			}
		}
	}
}
