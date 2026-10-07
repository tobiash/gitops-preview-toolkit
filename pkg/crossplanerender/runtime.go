package crossplanerender

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/containerd/errdefs"
	cprender "github.com/crossplane/cli/v2/cmd/crossplane/render"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

func ownedContainerName(sessionID, key string) string {
	hash := sha256.Sum256([]byte(key))
	return "fmp-crossplane-" + sessionID + "-" + hex.EncodeToString(hash[:6])
}

func removeOwnedContainer(ctx context.Context, name string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	err = cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Service) startRuntime(ctx context.Context, sess *session, rt cprender.Runtime, key string) (
	cprender.RuntimeContext, error,
) {
	docker, isDocker := rt.(*cprender.RuntimeDocker)
	if isDocker {
		// Plugin-owned names isolate permadiff sides and let us clean up if
		// upstream Start creates a container but fails before returning Stop.
		docker.Name = ownedContainerName(sess.id, key)
	}
	rctx, err := rt.Start(ctx)
	if !isDocker {
		return rctx, err
	}
	cleanup := func(ctx context.Context) error { return s.removeContainer(ctx, docker.Name) }
	if err == nil {
		stop := rctx.Stop
		rctx.Stop = func(ctx context.Context) error {
			if stop == nil {
				return cleanup(ctx)
			}
			if err := stop(ctx); err != nil {
				// A timed-out graceful stop or an already removed container
				// should not lose ownership. Force-removal is also idempotent.
				if cleanupErr := cleanup(ctx); cleanupErr != nil {
					return errors.Join(err, cleanupErr)
				}
			}
			return nil
		}
		return rctx, nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if cleanupErr := cleanup(cleanupCtx); cleanupErr != nil {
		// Retain a cleanup-only handle so session shutdown retries removal.
		// This entry consumes the same bounded cache slot as a live runtime.
		sess.runtimes[key] = cprender.RuntimeContext{Stop: cleanup}
		return rctx, errors.Join(err, fmt.Errorf("clean failed function startup: %w", cleanupErr))
	}
	return rctx, err
}
