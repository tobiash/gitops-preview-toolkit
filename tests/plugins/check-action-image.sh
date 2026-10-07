#!/usr/bin/env bash
set -euo pipefail

ls -d /tmp/opencode >/dev/null
bundle=$(mktemp -d /tmp/opencode/plugins-action-image.XXXXXX)
image="localhost/gitops-preview-packaging:${bundle##*.}"
label="gitops-preview.packaging-test=$bundle"
cleanup() {
  # Only our uniquely labelled containers and uniquely tagged image are removed.
  ids=$(podman ps -aq --filter "label=$label")
  if [ -n "$ids" ]; then podman rm -f $ids; fi
  podman image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$bundle"
}
trap cleanup EXIT
export TMPDIR="$bundle"
podman build --layers=false --force-rm --label "$label" --tag "$image" --file Dockerfile.action .
podman run --rm --label "$label" --entrypoint /bin/sh "$image" -ec 'for binary in fmp gitops-preview gitops-preview-flux gitops-preview-crossplane; do command -v "$binary"; done; gitops-preview version; fmp version'
mkdir "$bundle/fixture"
cp tests/plugins/fixtures/action/configmap.yaml "$bundle/fixture/configmap.yaml"
cp tests/plugins/fixtures/action/.gitops-preview.yaml "$bundle/fixture/.gitops-preview.yaml"
podman run --rm --label "$label" --volume "$bundle/fixture:/fixture:ro" --entrypoint /usr/local/bin/gitops-preview "$image" render /fixture --output json > "$bundle/render.json"
python3 -c 'import json,sys; doc=json.load(open(sys.argv[1])); assert doc["kind"] == "List"; assert len(doc["items"]) == 1; assert doc["items"][0]["metadata"]["name"] == "action-packaging"' "$bundle/render.json"
