#!/usr/bin/env bash
set -euo pipefail

# Validation only: no release, git mutation, or repository build output.
repo=$(git rev-parse --show-toplevel)
ls -d /tmp/opencode >/dev/null
bundle=$(mktemp -d /tmp/opencode/plugins-packaging.XXXXXX)
trap 'rm -rf "$bundle"' EXIT
export TMPDIR="$bundle" GOCACHE="$bundle/cache" CGO_ENABLED=0
cp "$repo/go.mod" "$bundle/check.mod"
cp "$repo/go.sum" "$bundle/check.sum"
go list -buildvcs=false -mod=mod -modfile="$bundle/check.mod" -deps ./cmd/gitops-preview ./cmd/fmp > "$bundle/core-dependencies.txt"
while IFS= read -r dependency; do
  case "$dependency" in
    helm.sh/helm/*|github.com/fluxcd/*|sigs.k8s.io/kustomize/api/krusty|github.com/tobiash/gitops-preview-toolkit/pkg/build*|github.com/tobiash/gitops-preview-toolkit/pkg/fluxrender*|github.com/tobiash/gitops-preview-toolkit/pkg/crossplanerender*|github.com/tobiash/gitops-preview-toolkit/pkg/expander*)
      exit 1 ;;
  esac
done < "$bundle/core-dependencies.txt"
for os in linux darwin; do
  for arch in amd64 arm64; do
    for binary in gitops-preview fmp gitops-preview-flux gitops-preview-crossplane; do
      GOOS="$os" GOARCH="$arch" go build -p=2 -buildvcs=false -mod=mod -modfile="$bundle/check.mod" -o "$bundle/$binary-$os-$arch" "./cmd/$binary"
      file "$bundle/$binary-$os-$arch"
    done
    # Cross-target caches are large; keep only the current target's cache.
    go clean -cache
  done
done

# Pin an official GoReleaser binary and verify its published checksum. check only
# validates configuration; it does not build, publish, create tags, or releases.
release=https://github.com/goreleaser/goreleaser/releases/download/v2.18.2
archive=goreleaser_Linux_x86_64.tar.gz
curl -fL --retry 2 "$release/$archive" -o "$bundle/$archive"
curl -fL --retry 2 "$release/checksums.txt" -o "$bundle/checksums.txt"
expected=
while read -r hash name; do
  if [ "$name" = "$archive" ]; then expected="$hash"; break; fi
done < "$bundle/checksums.txt"
test -n "$expected"
actual=$(sha256sum "$bundle/$archive")
test "${actual%% *}" = "$expected"
tar -xzf "$bundle/$archive" -C "$bundle" goreleaser
"$bundle/goreleaser" check --config "$repo/.goreleaser.yml"
