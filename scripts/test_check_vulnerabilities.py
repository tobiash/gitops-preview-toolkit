"""Regression tests for the security gate's trust boundaries (standard library only)."""

import copy
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("gate", Path(__file__).with_name("check-vulnerabilities.py"))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


def advisory(identifier):
    return {"id": identifier, "database_specific": {"review_status": "UNREVIEWED"}, "affected": [{
        "package": {"name": gate.DOCKER, "ecosystem": "Go"},
        "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}]}],
        "ecosystem_specific": {},
    }]}


def finding(identifier="GO-2026-4887", **changes):
    frame = {"module": gate.DOCKER, "version": gate.VERSION, "package": gate.DOCKER + "/client", "function": "NewClientWithOpts"}
    frame.update(changes)
    return {"osv": identifier, "trace": [frame]}


def stream(findings, advisories=None):
    if advisories is None:
        advisories = [advisory(i) for i in sorted({f["osv"] for f in findings})]
    messages = [{"config": {"protocol_version": "v1.0.0", "scan_level": "symbol", "scan_mode": "source"}}, {"SBOM": {"modules": []}}]
    messages += [{"osv": a} for a in advisories]
    messages += [{"finding": f} for f in findings]
    return "".join(json.dumps(m, indent=2) for m in messages)


class GateTests(unittest.TestCase):
    def evaluate(self, findings, advisories=None, code=0, graphs=True):
        return gate.evaluate(stream(findings, advisories), code, graphs)

    def assert_failure(self, findings, **kwargs):
        counts = self.evaluate(findings, **kwargs)
        self.assertTrue(any(disposition.startswith("FAIL") for _, disposition in counts))

    def test_two_exact_exceptions_with_exit_zero(self):
        counts = self.evaluate([finding(i) for i in gate.EVIDENCE])
        self.assertEqual(sum(counts.values()), 2)
        self.assertTrue(all(d.startswith("EXCEPTED") for _, d in counts))

    def test_contiguous_pretty_json(self):
        self.assertEqual(gate.objects(' {\n"a": 1\n}{"b": 2} '), [{"a": 1}, {"b": 2}])

    def test_malformed_streams(self):
        for text in ("", "[]", '{"a":1}garbage', '{"a":', '{"a":1,"a":2}'):
            with self.subTest(text=text), self.assertRaises(gate.GateError):
                gate.objects(text)

    def test_config_protocol_and_scan_level(self):
        for key, value in (("protocol_version", "v2.0.0"), ("protocol_version", "v1.1.0"), ("scan_level", "module"), ("scan_mode", "binary")):
            messages = gate.objects(stream([]))
            messages[0]["config"][key] = value
            with self.subTest(key=key, value=value), self.assertRaises(gate.GateError):
                gate.evaluate("".join(map(json.dumps, messages)), 0, True)
        with self.assertRaises(gate.GateError):
            gate.evaluate('{"progress":{}}', 0, True)

    def test_nonzero_scan_status(self):
        for code in (1, 2, -9):
            with self.subTest(code=code), self.assertRaises(gate.GateError):
                self.evaluate([finding()], code=code)

    def test_error_message(self):
        with self.assertRaises(gate.GateError):
            gate.evaluate(stream([]) + '{"error":{"message":"scan failed"}}', 0, True)

    def test_module_or_version_change(self):
        for changes in ({"version": "v28.5.3+incompatible"}, {"version": ""}, {"module": "github.com/moby/moby"}, {"module": "github.com/moby/moby/v2"}):
            with self.subTest(changes=changes):
                self.assert_failure([finding(**changes)])

    def test_path_segment_boundaries(self):
        for suffix in ("/daemon", "/pkg/authorization", "/plugin", "/api/typesExtra", "/client/daemon", "/pkg/stdcopyevil", "/api/server"):
            with self.subTest(suffix=suffix):
                self.assert_failure([finding(package=gate.DOCKER + suffix)])
        for suffix in ("/api", "/api/types", "/api/types/container", "/client", "/pkg/stdcopy"):
            self.assertTrue(gate.approved(gate.DOCKER + suffix))

    def test_unknown_advisory_not_excepted(self):
        self.assert_failure([finding("GO-2026-9999")])

    def test_genuine_grpc_finding_fails_even_exit_zero(self):
        self.assert_failure([finding(), finding("GO-2026-9999", module="google.golang.org/grpc", version="v1.83.1", package="google.golang.org/grpc", function="Serve")])

    def test_package_only_finding_fails(self):
        self.assert_failure([finding("GO-2026-9999", function="")])

    def test_module_only_informational(self):
        counts = self.evaluate([finding("GO-2026-9999", package="", function="")])
        self.assertTrue(all(d.startswith("INFORMATIONAL") for _, d in counts))

    def test_invalid_graph_cancels_exception(self):
        self.assert_failure([finding()], graphs=False)

    def test_selected_module_requires_original_version_no_replace(self):
        for module in ({"Path": gate.DOCKER, "Version": "v29.0.0"}, {"Path": gate.DOCKER, "Version": gate.VERSION, "Replace": {}}, {"Path": "github.com/moby/moby", "Version": gate.VERSION}):
            with self.subTest(module=module), self.assertRaises(gate.GateError):
                gate.module_check(module)

    def test_daemon_import_invalidates_review(self):
        module = {"Path": gate.DOCKER, "Version": gate.VERSION}
        for suffix in ("/plugin", "/pkg/authorization", "/daemon"):
            with self.subTest(suffix=suffix), self.assertRaises(gate.GateError):
                gate.graph_check([{"ImportPath": gate.DOCKER + "/client", "Module": module}, {"ImportPath": gate.DOCKER + suffix, "Module": module}])

    def test_incomplete_graph(self):
        with self.assertRaises(gate.GateError):
            gate.graph_check([{"ImportPath": "example.org/pkg", "DepsErrors": [{"Err": "missing"}]}])

    def test_metadata_requires_renewed_review(self):
        original = advisory("GO-2026-4887")
        variants = []
        for key, value in (("ecosystem_specific", {"packages": [{"path": gate.DOCKER + "/client"}]}), ("ranges", [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "28.5.3"}]}]), ("versions", ["28.5.2"])):
            changed = copy.deepcopy(original)
            changed["affected"][0][key] = value
            variants.append(changed)
        changed = copy.deepcopy(original)
        changed["database_specific"]["review_status"] = "REVIEWED"
        variants.append(changed)
        changed = copy.deepcopy(original)
        del changed["database_specific"]
        variants.append(changed)
        for changed in variants:
            with self.subTest(changed=changed), self.assertRaises(gate.GateError):
                self.evaluate([finding()], advisories=[changed])

    def test_missing_metadata_and_trace(self):
        with self.assertRaises(gate.GateError):
            self.evaluate([finding()], advisories=[])
        malformed = finding()
        malformed["trace"] = []
        with self.assertRaises(gate.GateError):
            self.evaluate([malformed])

    def test_repeated_osv_requires_identical_metadata(self):
        original = advisory("GO-2026-4887")
        self.evaluate([finding()], advisories=[original, copy.deepcopy(original)])
        changed = copy.deepcopy(original)
        changed["details"] = "changed"
        with self.assertRaises(gate.GateError):
            self.evaluate([finding()], advisories=[original, changed])

    def test_incomplete_config_only_scan(self):
        config = gate.objects(stream([]))[0]
        with self.assertRaises(gate.GateError):
            gate.evaluate(json.dumps(config), 0, True)

    def test_all_release_targets_and_native_tests_checked(self):
        module = {"Path": gate.DOCKER, "Version": gate.VERSION}
        with patch.object(gate, "run_json", side_effect=[[module]] + [[{"ImportPath": gate.DOCKER + "/client", "Module": module}]] * 5) as run:
            gate.validate_graphs()
        self.assertEqual(run.call_count, 6)
        environments = [call.args[1] for call in run.call_args_list[1:5]]
        self.assertEqual({(env["GOOS"], env["GOARCH"], env["CGO_ENABLED"]) for env in environments}, {(system, arch, "0") for system in ("linux", "darwin") for arch in ("amd64", "arm64")})
        self.assertIn("-test", run.call_args_list[-1].args[0])


if __name__ == "__main__":
    unittest.main()
