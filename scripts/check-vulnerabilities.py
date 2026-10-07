#!/usr/bin/env python3
"""Fail-closed govulncheck gate; reviewed Docker SDK scope is documented in docs/security."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
from collections import Counter


DOCKER = "github.com/docker/docker"
VERSION = "v28.5.2+incompatible"
EVIDENCE = {
    "GO-2026-4887": "https://github.com/moby/moby/commit/e89edb19ad7de0407a5d31e3111cb01aa10b5a38",
    "GO-2026-4883": "https://github.com/moby/moby/commit/f4d6f25bf0c3fa12d4968320a45685947756a22a",
}


class GateError(ValueError):
    """Invalid scan or invalidated review evidence."""


def objects(text):
    """Decode contiguous, potentially pretty-printed JSON objects, not NDJSON."""
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise GateError(f"duplicate JSON key: {key}")
            result[key] = value
        return result

    decoder = json.JSONDecoder(object_pairs_hook=unique)
    offset = 0
    result = []
    while offset < len(text):
        if text[offset].isspace():
            offset += 1
            continue
        try:
            value, offset = decoder.raw_decode(text, offset)
        except json.JSONDecodeError as exc:
            raise GateError(f"malformed JSON stream: {exc}") from exc
        if not isinstance(value, dict):
            raise GateError("JSON stream must contain objects")
        result.append(value)
    if not result:
        raise GateError("empty JSON stream")
    return result


def approved(package):
    return package in {DOCKER + "/api", DOCKER + "/client", DOCKER + "/pkg/stdcopy", DOCKER + "/api/types"} or package.startswith(DOCKER + "/api/types/")


def module_check(module):
    if module.get("Path") != DOCKER or module.get("Version") != VERSION or "Replace" in module:
        raise GateError("Docker review requires the original v28.5.2+incompatible module without Replace")


def graph_check(packages):
    found = set()
    for package in packages:
        if package.get("Error") or package.get("DepsErrors") or package.get("Incomplete"):
            raise GateError("incomplete dependency graph")
        path = package.get("ImportPath")
        if not isinstance(path, str):
            raise GateError("dependency graph missing ImportPath")
        module = package.get("Module", {})
        if module.get("Path") == DOCKER or path == DOCKER or path.startswith(DOCKER + "/"):
            module_check(module)
            if not approved(path):
                raise GateError(f"Docker package outside reviewed SDK scope: {path}")
            found.add(path)
    return found


def metadata_check(osv):
    targets = [a for a in osv.get("affected", []) if a.get("package", {}).get("name") == DOCKER]
    expected = {
        "package": {"name": DOCKER, "ecosystem": "Go"},
        "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}]}],
        "ecosystem_specific": {},
    }
    # Compare the entire target entry: packages, symbols, fixed ranges or new fields
    # require renewed review. Other affected modules are never excepted.
    if targets != [expected] or osv.get("withdrawn"):
        raise GateError(f"{osv.get('id')}: advisory scope changed; renewed review required")
    if osv.get("database_specific", {}).get("review_status") != "UNREVIEWED":
        raise GateError(f"{osv.get('id')}: review status changed or missing; renewed review required")


def evaluate(text, returncode, graphs_valid):
    if returncode != 0:
        raise GateError(f"govulncheck failed with exit status {returncode}")
    messages = objects(text)
    config = messages[0].get("config", {})
    if not re.fullmatch(r"v?1\.0\.\d+", str(config.get("protocol_version", ""))):
        raise GateError("missing or incompatible govulncheck protocol (requires 1.0.x)")
    if config.get("scan_level") != "symbol" or config.get("scan_mode") != "source":
        raise GateError("requires a source, symbol-level scan")
    advisories = {}
    findings = []
    has_sbom = False
    for index, message in enumerate(messages):
        if len(message) != 1:
            raise GateError("invalid govulncheck message envelope")
        kind, value = next(iter(message.items()))
        if kind not in {"config", "SBOM", "sbom", "progress", "osv", "finding"} or not isinstance(value, dict):
            raise GateError(f"unexpected govulncheck message: {kind}")
        if kind == "config" and index != 0:
            raise GateError("duplicate scan config")
        if kind in {"SBOM", "sbom"}:
            has_sbom = True
        if kind == "osv":
            identifier = value.get("id")
            if not isinstance(identifier, str) or not identifier:
                raise GateError("missing advisory ID")
            # The scanner emits an OSV again when it affects multiple modules.
            if identifier in advisories and advisories[identifier] != value:
                raise GateError(f"{identifier}: conflicting advisory metadata")
            advisories[identifier] = value
        if kind == "finding":
            findings.append(value)
    if not has_sbom:
        raise GateError("incomplete scan: missing SBOM")
    counts = Counter()
    for finding in findings:
        identifier = finding.get("osv")
        if not isinstance(identifier, str) or identifier not in advisories:
            raise GateError("finding lacks advisory metadata")
        trace = finding.get("trace")
        if not isinstance(trace, list) or not trace or any(not isinstance(f, dict) for f in trace):
            raise GateError(f"{identifier}: invalid finding trace")
        frame = trace[0]
        if not isinstance(frame.get("module"), str) or not frame["module"]:
            raise GateError(f"{identifier}: missing vulnerable module")
        for key in ("package", "function", "receiver", "version"):
            if key in frame and not isinstance(frame[key], str):
                raise GateError(f"{identifier}: invalid {key}")
        package = frame.get("package", "")
        if not package:
            if frame.get("function") or frame.get("receiver"):
                raise GateError(f"{identifier}: symbol without package")
            counts[identifier, "INFORMATIONAL module-only"] += 1
        elif identifier in EVIDENCE and frame["module"] == DOCKER and frame.get("version") == VERSION and approved(package) and graphs_valid:
            metadata_check(advisories[identifier])
            counts[identifier, "EXCEPTED reviewed Docker SDK false positive"] += 1
        else:
            counts[identifier, "FAIL package/symbol finding"] += 1
    return counts


def run_json(command, env=None):
    result = subprocess.run(command, env=env, capture_output=True, text=True)
    if result.stderr:
        print(result.stderr, file=sys.stderr, end="")
    if result.returncode:
        raise GateError(f"{' '.join(command)} failed with exit status {result.returncode}")
    return objects(result.stdout)


def validate_graphs():
    selected = run_json(["go", "list", "-mod=readonly", "-m", "-json", DOCKER])
    if len(selected) != 1:
        raise GateError("expected one selected Docker module")
    module_check(selected[0])
    for system in ("linux", "darwin"):
        for arch in ("amd64", "arm64"):
            env = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED="0")
            packages = run_json(["go", "list", "-mod=readonly", "-buildvcs=false", "-deps", "-json", "./cmd/..."], env)
            found = graph_check(packages)
            print(f"GRAPH {system}/{arch} CGO_ENABLED=0: {len(found)} approved Docker SDK packages", file=sys.stderr)
    packages = run_json(["go", "list", "-mod=readonly", "-buildvcs=false", "-deps", "-test", "-json", "./..."])
    found = graph_check(packages)
    print(f"GRAPH native tests: {len(found)} approved Docker SDK packages", file=sys.stderr)


def main():
    os.chdir(Path(__file__).resolve().parents[1])
    # Always preserve scanner output, even when graph validation cancels review.
    result = subprocess.run(["govulncheck", "-json", "./..."], capture_output=True, text=True)
    sys.stdout.write(result.stdout)
    sys.stdout.flush()
    sys.stderr.write(result.stderr)
    try:
        validate_graphs()
        counts = evaluate(result.stdout, result.returncode, graphs_valid=True)
        for (identifier, disposition), count in sorted(counts.items()):
            print(f"{identifier}: {disposition} ({count} findings); https://pkg.go.dev/vuln/{identifier}", file=sys.stderr)
            if disposition.startswith("EXCEPTED"):
                print(f"  Source evidence: {EVIDENCE[identifier]}", file=sys.stderr)
        failures = sum(n for (_, disposition), n in counts.items() if disposition.startswith("FAIL"))
        print(f"GATE: {'FAIL' if failures else 'PASS'}; {failures} residual package/symbol findings", file=sys.stderr)
        return 1 if failures else 0
    except (GateError, OSError, TypeError, KeyError, AttributeError) as exc:
        print(f"GATE: FAIL: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
