package crossplanerender

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cprender "github.com/crossplane/cli/v2/cmd/crossplane/render"
	"github.com/go-logr/logr"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func TestCloseRetriesFailedRuntimeCleanup(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"session", "service"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			service := New(logr.Discard())
			id := openTestSession(t, service)
			sess := service.sessions[id]
			stops := 0
			sess.runtimes["failed-once"] = cprender.RuntimeContext{Stop: func(ctx context.Context) error {
				stops++
				if stops == 1 {
					return errors.New("daemon unavailable")
				}
				return nil
			}}
			close := func() error {
				if mode == "service" {
					return service.Close()
				}
				_, err := service.CloseRender(t.Context(), &plugin.CloseRequest{Session: id})
				return err
			}
			if err := close(); err == nil {
				t.Fatal("failed cleanup reported success")
			}
			if service.sessions[id] != sess || len(sess.runtimes) != 1 {
				t.Fatal("failed cleanup lost session or runtime ownership")
			}
			if _, err := service.Expand(t.Context(), &plugin.ExpandRequest{Session: id}); err == nil {
				t.Fatal("closed cleanup-pending session accepted rendering")
			}
			if err := close(); err != nil {
				t.Fatal(err)
			}
			if stops != 2 || len(sess.runtimes) != 0 || service.sessions[id] != nil {
				t.Fatal("close did not retry failed cleanup")
			}
			if err := close(); err != nil || stops != 2 {
				t.Fatal("successful close was not idempotent")
			}
		})
	}
}

func TestFreshDevelopmentRejected(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"runtime":"Development","developmentTargets":{"templating":"127.0.0.1:9443"}}`,
		`{"runtime":"Docker","developmentTargets":{"templating":"127.0.0.1:9443"}}`,
	} {
		service := New(logr.Discard())
		_, err := service.OpenRender(t.Context(), &plugin.OpenRequest{Fresh: true, Config: []byte(raw)})
		assertCode(t, err, "fresh-development-unsupported")
		opened, err := service.OpenRender(t.Context(), &plugin.OpenRequest{Fresh: false, Config: []byte(raw)})
		if err != nil || opened.Session == "" {
			t.Fatalf("ordinary Development rejected: %v", err)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPartialDockerStartupRetainsCleanupHandle(t *testing.T) {
	// Exercise the official Docker runtime's create-success/start-failure path
	// against a local Docker API fixture. No container daemon is needed.
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.51")
			_, _ = fmt.Fprint(w, "OK")
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"Id":"partial-container"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"message":"failed to start"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"no such container"}`)
		}
	}))
	defer daemon.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(daemon.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.51")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	service := New(logr.Discard())
	id := openTestSession(t, service)
	sess := service.sessions[id]
	removals := 0
	service.removeContainer = func(ctx context.Context, name string) error {
		if name != ownedContainerName(id, "partial") {
			t.Fatalf("cleanup did not use trusted session-owned name: %s", name)
		}
		removals++
		if removals == 1 {
			return errors.New("first cleanup failed")
		}
		return nil
	}
	cfg, _, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	fn, err := effectiveFunction(testResource(t, testFunction).object, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fn.Spec.Package = "example.invalid/fixture:v1"
	fn.Annotations[cprender.AnnotationKeyRuntimeDockerPullPolicy] = "Never"
	rt, err := cprender.GetRuntimeDocker(fn, service.log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.startRuntime(t.Context(), sess, rt, "partial"); err == nil {
		t.Fatal("failed Docker startup reported success")
	}
	if len(sess.runtimes) != 1 || sess.runtimes["partial"].Stop == nil {
		t.Fatal("partial startup cleanup handle lost")
	}
	if _, err := service.CloseRender(t.Context(), &plugin.CloseRequest{Session: id}); err != nil {
		t.Fatal(err)
	}
	if removals != 2 || len(sess.runtimes) != 0 {
		t.Fatal("partial startup cleanup was not retried")
	}
}
