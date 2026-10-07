# Reviewed Docker client advisory scope

Reviewed for v0.2.0-rc.1 on 2026-10-08. The release uses the original
`github.com/docker/docker v28.5.2+incompatible` as an SDK dependency, without a
module replacement. It does not bundle Docker Engine.

## Source evidence

| Go advisory | Vulnerable implementation | Upstream evidence |
| --- | --- | --- |
| [GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887) / CVE-2026-34040 | `pkg/authorization.(*Ctx).AuthZRequest` and `drainBody`: daemon authorization-plugin request-body handling | [Advisory](https://github.com/moby/moby/security/advisories/GHSA-x744-4wpc-v9h2), [fix](https://github.com/moby/moby/commit/e89edb19ad7de0407a5d31e3111cb01aa10b5a38), [original implementation](https://github.com/moby/moby/blob/v28.5.2/pkg/authorization/authz.go) |
| [GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883) / CVE-2026-33997 | `plugin.validatePrivileges` and `isEqual`: daemon plugin-install privilege validation | [Advisory](https://github.com/moby/moby/security/advisories/GHSA-pxq6-2prw-chj9), [fix](https://github.com/moby/moby/commit/f4d6f25bf0c3fa12d4968320a45685947756a22a), [original implementation](https://github.com/moby/moby/blob/v28.5.2/plugin/manager.go) |

The independently reviewed vulnerable implementations are absent from every
release command dependency graph. The SDK graph contains 23 Docker packages:
`api`, `api/types` and its subpackages, `client`, and `pkg/stdcopy`. Neither daemon
authorization code nor daemon plugin installation is included. Calling a Docker
client API is not executing the server's authorization or privilege-validation
implementation in the toolkit process.

The Go database currently marks these two entries `UNREVIEWED`. Their Docker
affected entries cover the entire module (`SEMVER`, introduced `0`, no fixed
version) and have empty `ecosystem_specific` metadata, with no package or symbol
scope. Consequently govulncheck reports SDK functions as vulnerable even though
the implementations identified by the upstream advisories are absent. This is a
review of these exact two false positives, not an exemption for unreviewed
advisories generally.

## Executable security gate

Run from the checkout:

```sh
python3 -m unittest discover -s scripts -p 'test_check_vulnerabilities.py'
python3 scripts/check-vulnerabilities.py > govulncheck.json
```

The wrapper invokes `govulncheck -json ./...`, preserves its complete raw JSON
object stream on stdout, and prints advisory dispositions, evidence links and
the final gate result on stderr. The output is contiguous, often pretty-printed
JSON objects, not a single JSON array or newline-delimited JSON. Redirect stdout
to retain an audit artifact if needed.

Exceptions require all of the following:

- Protocol 1.0.x, source-mode symbol scan, successful scanner exit, valid messages
  and matching OSV metadata. Missing review status fails closed; the actual
  current JSON includes `database_specific.review_status`.
- Exactly one selected Docker module at `v28.5.2+incompatible`, with no `Replace`.
- Complete `go list -mod=readonly -buildvcs=false -deps -json ./cmd/...` graphs
  for Linux/macOS, amd64/arm64, `CGO_ENABLED=0`, and a native `-test ./...` graph.
  Every Docker package must be within the approved SDK paths. `api/types/` uses
  a path-segment boundary; `api/server`, `client/…`, daemon and plugin packages
  are not approved. Any violation cancels the exception and fails the gate.
- The finding's **first** trace frame (the vulnerable frame) must name the exact
  Docker module/version and an approved package; a later client caller cannot
  exempt a vulnerable daemon frame. Neither Moby module path is exempted.
- The exact Docker OSV affected entry must still contain only the reviewed
  package/ecosystem, open-ended SEMVER range and empty ecosystem metadata, and
  remain `UNREVIEWED`. Added package/symbol scope, fixed ranges, replacements,
  versions or changed review status require renewed review.

All other package or symbol findings fail, including new advisory IDs and
findings emitted with JSON-mode exit status zero. Module-only findings remain
visible and informational, consistent with the default govulncheck gate not
failing for required-module presence alone. Scan errors, nonzero exits, malformed
streams and unsupported protocols fail independently of advisory exceptions.

## External Docker Engine

The exception does not patch Docker or assert that an external runtime is safe.
If using a susceptible external Docker Engine for composition functions, update
that daemon to a patched version according to the upstream advisories. Its
authorization-plugin and plugin-install behavior remains outside the bundled
SDK graph. Re-evaluate this review whenever the SDK version, imported packages,
release targets or advisory scope changes.
