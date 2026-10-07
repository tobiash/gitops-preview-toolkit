#!/usr/bin/env bash
set -euo pipefail

# All downloads, build output, module updates and runtime fixtures stay outside
# the checkout. The Go test supervises the actual function and plugin processes.
repo=$(git rev-parse --show-toplevel)
ls -d /tmp/opencode >/dev/null
bundle=$(mktemp -d /tmp/opencode/plugins-live-build.XXXXXX)
podman_pid=
cleanup() {
  if [ -n "$podman_pid" ]; then
    kill "$podman_pid" 2>/dev/null || true
    wait "$podman_pid" 2>/dev/null || true
  fi
  rm -rf "$bundle"
}
trap cleanup EXIT
export TMPDIR="$bundle"
export GOCACHE=/tmp/opencode/plugins-go-build-cache
url=https://releases.crossplane.io/stable/v2.4.2/bin/linux_amd64/crossplane
curl -fL --retry 2 "$url" -o "$bundle/crossplane"
curl -fL --retry 2 "$url.sha256" -o "$bundle/crossplane.sha256"
expected=$(tr -d '[:space:]' < "$bundle/crossplane.sha256")
actual=$(sha256sum "$bundle/crossplane")
test "${actual%% *}" = "$expected"
chmod +x "$bundle/crossplane"
test "$("$bundle/crossplane" --version)" = v2.4.2
GOBIN="$bundle" go install github.com/crossplane-contrib/function-go-templating@v0.12.0
cp "$repo/go.mod" "$bundle/live.mod"
cp "$repo/go.sum" "$bundle/live.sum"
go build -buildvcs=false -mod=mod -modfile="$bundle/live.mod" -o "$bundle/gitops-preview-flux" ./cmd/gitops-preview-flux
go build -buildvcs=false -mod=mod -modfile="$bundle/live.mod" -o "$bundle/gitops-preview-crossplane" ./cmd/gitops-preview-crossplane
go build -buildvcs=false -mod=mod -modfile="$bundle/live.mod" -o "$bundle/gitops-preview" ./cmd/gitops-preview
export CROSSPLANE_TEST_ENGINE="$bundle/crossplane"
export CROSSPLANE_TEST_TEMPLATING_BINARY="$bundle/function-go-templating"
export PLUGIN_TEST_FLUX_BINARY="$bundle/gitops-preview-flux"
export PLUGIN_TEST_CROSSPLANE_BINARY="$bundle/gitops-preview-crossplane"
export PLUGIN_TEST_CORE_BINARY="$bundle/gitops-preview"
if [ "${PLUGIN_TEST_DOCKER:-0}" = 1 ]; then
  export DOCKER_HOST="unix://$bundle/podman.sock"
  podman system service --time=0 "$DOCKER_HOST" > "$bundle/podman.log" 2>&1 &
  podman_pid=$!
  ready=0
  for attempt in {1..100}; do
    if curl --silent --fail --unix-socket "$bundle/podman.sock" http://localhost/_ping; then ready=1; break; fi
    sleep 0.1
  done
  test "$ready" = 1
  podman pull ghcr.io/crossplane-contrib/function-go-templating:v0.12.0
fi
go test -mod=mod -modfile="$bundle/live.mod" -tags=integration ./tests/plugins -run TestLive -count=1 -v -timeout=5m
