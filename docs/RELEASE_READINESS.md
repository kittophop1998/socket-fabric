# Phase 4D — Release preparation and distribution readiness

Status: **BLOCKED for public release**. Local preparation is implemented; the
verification evidence below separates it from public installation. No publication,
tags, public binaries, account changes or deployment
are authorized by this preparation phase.

## Ownership and identity audit

| Item | Status | Evidence / required decision |
| --- | --- | --- |
| Git repository, remote, commit and history | BLOCKED | This checkout has an empty read-only `.git` placeholder; Git reports that it is not a repository. Recover/clone the owner-confirmed repository before release. |
| Root module | BLOCKED | `socketfabric` is a local identity without a verified public VCS origin. |
| Go SDK module | BLOCKED | `socketfabric/sdk/go`; no remotely accessible tagged module is confirmed. Preserve imports until ownership is verified. |
| Example module | PASS | Separate `socketfabric/examples/sdk` module; imports the public SDK only. |
| Local replacements/workspace | PASS | Root/example replacements support checkout development. Phase 4C's independent fixture uses a copied SDK through a temporary workspace. This does not prove public installation. |
| npm identity | BLOCKED | `@socketfabric/client` at private version `0.1.0` is provisional. On 2026-10-16 `npm whoami` returned 401 and public package lookup returned 404; neither proves ownership or name availability. |
| License | BLOCKED | No project LICENSE was found. Owner approval is required before selecting terms or adding npm license metadata. |
| Public installation | NOT VERIFIED | No `go get` from a public tag or npm registry installation is claimed. |
| Existing CI | NOT VERIFIED | No CI configuration was found; local scripts will provide a provider-independent gate without assuming a hosting account. |

## Implementation plan

1. Reuse the Makefile and Node/Go toolchains for local CLI cross-builds, stable
   metadata, artifact manifests, SHA-256 checksums and a second-build comparison.
2. Build, dry-pack, pack and inspect the npm artifact; retain `private: true`
   and the current version. Reject unexpected package files and credentials.
3. Reuse the existing external/browser runner with optional artifact inputs so
   the generated CLI and npm package are exercised in real single/distributed
   flows rather than substituting fresh test builds.
4. Add a reproducible local/CI check script for root/nested Go modules, SDK/demo
   checks, artifacts, external consumers, real NATS/browser integration and
   documentation. Record security scan limits and actual dependency findings.
5. Document compatibility/SemVer proposal, manual release gates, installation,
   contribution/security guidance and an unreleased changelog. Keep ownership,
   licensing, public install and native platform checks explicitly blocked or
   unverified where appropriate.

## Verification results

Evidence date: **2026-10-16**. Host: Linux amd64, Go **1.27.2**, Node **24.18.0**,
npm **11.16.0**, official govulncheck **v1.9.0**, real NATS **2.15.1** and Chromium.

| Component | Local status | Public distribution status |
| --- | --- | --- |
| Core | PASS — root tests/race/build, both Compose modes and real NATS/browser flows | BLOCKED — license, Git identity/provenance and public version decisions |
| Go SDK | PASS — independent module tests/race, context/errors/concurrency, copied external workspace consumer and dependency boundaries | BLOCKED — canonical module path/tag; clean public `go get` NOT VERIFIED |
| TypeScript SDK | PASS — strict tests/build, npm dry-run/pack, artifact contents and independent installed/bundled consumer/SSR | BLOCKED — scope ownership/license/metadata; registry install NOT VERIFIED |
| CLI | PASS — six cross-build targets, repeated-build hashes, checksums/notices, native standalone real-gateway commands | BLOCKED — public provenance/license/version; native macOS/Windows/arm64 execution NOT VERIFIED |

| Check | Actual result |
| --- | --- |
| Root + nested Go formatting/tidy/cache verification/vet/unit/race/build | PASS; local SDK requirement handled separately from downloaded cache verification |
| Fresh real-NATS adapter/bootstrap integration with `-count=1 -race` | PASS |
| TypeScript SDK strict checks/tests | PASS, 25 tests |
| Existing generated demo and lifecycle tests | PASS, 9 tests |
| Release tooling security tests | PASS, 5 tests; metadata injection, environment stripping, tar bounds/traversal/links, redaction, checksum tamper/coverage and symlink rejection |
| npm audit, all three locked graphs | PASS, zero reported findings |
| govulncheck, root/SDK/example on Go 1.27.2 | PASS, zero reachable findings; root module-only OpenPGP advisory disclosed below |
| CLI Linux/macOS/Windows amd64 + arm64 | PASS, actual supported Go targets/build metadata checked; two builds per target from separate temporary source paths |
| npm `pack --dry-run`, actual pack twice and bounded package inspection | PASS, exact file set, zero runtime dependencies, matching repeated tarball hashes |
| Packed artifact installed into independent consumer | PASS, ESM/NodeNext types/SSR and real browser bundle without source aliases or Node runtime dependencies |
| Standalone native CLI from an external directory | PASS, version/help/config and real status/ping/publish/auth/outage failures through existing API |
| Actual CLI/npm artifacts in Docker/NATS/Chromium integration | PASS, both Compose image builds and single/distributed application workflows |
| Authorization/project-room isolation/reconnect/expiry/outage/shutdown | PASS, no routing-loop duplicates in exercised sessions; browser/server secret checks and isolated process/container/network cleanup passed |
| Documentation links | PASS, updated local relative links resolve |
| Targeted current-source/package/binary scan | PASS within documented pattern/known-credential coverage; comprehensive history audit NOT VERIFIED |
| Hosted CI, real public installation, native foreign platforms, image OS scan, signatures | NOT VERIFIED |

The final gate recorded **20 local PASS checks**. `make release-verify` and the
system `sha256sum --check SHA256SUMS` also passed for all nine checksummed files.
The distribution contains eight payload artifacts plus manifest/checksum files.
Its npm package is **10,491 bytes packed / 37,201 bytes of package file content**,
10 allowlisted files and zero runtime dependencies. All six CLI binary hashes
and the npm/notice payload hashes also matched between the complete builds.
The initial task-generated copy was removed after comparison; the final local
distribution is `bin/release/`.

The machine-readable local gate summary is `bin/release-verification.json`;
artifact inputs/toolchain/size/checksum evidence is `bin/release/manifest.json`.
These ignored outputs are local development records, not public release or
ownership attestations. Tests are functional verification, not a load benchmark.

### Changed files

- `scripts/release.mjs`, `scripts/release.test.mjs`: local cross-build/package/
  checksum/notice/scan/verification tooling and security regression tests.
- `scripts/check.mjs`, `Makefile`: reproducible provider-independent quality gate
  and additional local build/verify/check targets; existing targets remain.
- `tests/sdk/run.mjs`, `tests/sdk/README.md`: optional actual CLI/npm artifact
  inputs; default Phase 4C behavior is preserved, including external Go fixtures.
- Root/nested `go.mod`, root `go.sum`, `Dockerfile`: Go security patch floor and
  the two advisory-fixed indirect dependencies; no public API or component
  release version change.
- `sdk/go/client.go`, `token.go`: exported constructor/config/token documentation
  comments; no SDK behavior change.
- `.gitignore`, `.dockerignore`: keep account credentials/config and build-only
  tooling/docs out of source control/build context where appropriate.
- `README.md`, `docs/CLI.md`, `docs/QUICKSTART.md`, `docs/PROJECT_CONTEXT.md`, both
  SDK READMEs and this report: current tooling, compatibility, security findings,
  ownership/licensing decisions and installation/release steps.
- `CONTRIBUTING.md`, `SECURITY.md`, `CHANGELOG.md`: useful contribution checks,
  trust/reporting boundaries and an Unreleased changelog; no project LICENSE.

## Local artifact workflow

Requirements: Go **1.27.2**, Node **24.3+** for the full existing demo suite, npm,
Docker Compose, Playwright Chromium and the pinned NATS server used by the
existing integration tests. SDK-only builds need Node 22+. Keep toolchain
versions fixed when comparing hashes; the manifest records actual versions.

```sh
npm --prefix sdk/typescript ci
node scripts/release.mjs build
node scripts/release.mjs verify
# Equivalent Make targets: make cli-dist release-verify
```

Output under ignored `bin/release/`:

- Six raw standalone CLI binaries for Linux/macOS/Windows amd64 and arm64.
- `npm/socketfabric-client-0.1.0.tgz`, retaining the current private package.
- `THIRD_PARTY_NOTICES.txt`, preserving existing Go LICENSE/PATENTS and the
  linked JWT library's license; it does not select SocketFabric's license.
- `manifest.json`: input hashes, versions, build metadata, file sizes and hashes.
- `SHA256SUMS`: every distributed file plus the manifest, using relative paths.

The script checks targets against the actual Go toolchain, builds each CLI twice
from separate temporary source paths with VCS stamping disabled, compares their
hashes, inspects target build metadata and packs npm twice. It checks dry-run vs
actual package contents, expands the bounded tarball without extracting files,
rejects traversal/links/unexpected files and scans unpacked content. It never
invokes a registry publish, tag, upload or deployment command.

Defaults are `VERSION=dev`, `COMMIT=unknown`, `SOURCE_DATE_EPOCH=0`; build time is
the epoch timestamp unless a valid UTC `BUILD_TIME` is supplied. These are local
development metadata, not a verified commit or release date. Do not use a live
secret as build metadata. For an owner-reviewed build, supply fixed values:

```sh
VERSION=dev COMMIT=unknown SOURCE_DATE_EPOCH=0 node scripts/release.mjs build --out bin/release-repeat
node scripts/release.mjs verify --out bin/release-repeat
```

The destination must not already exist. Nothing silently deletes or overwrites
an earlier distribution. Use a fresh directory for changed inputs; the full gate
can reuse an existing distribution only when its source input hashes match.
Reproducibility comparisons cover the recorded inputs/toolchain, not arbitrary
Go/npm/OS versions. No signature or authenticated provenance is generated.

The full provider-independent gate is documented in [CONTRIBUTING](../CONTRIBUTING.md).
It also runs the existing browser suite against these exact binaries/tarball:

```sh
NATS_SERVER_BIN=/absolute/path/to/nats-server \
  GOVULNCHECK_BIN=/absolute/path/to/govulncheck node scripts/check.mjs
# Or: make release-check (with those paths exported)
```

The Go vulnerability scanner used for this audit is the official
`golang.org/x/vuln/cmd/govulncheck` **v1.9.0**, installed as a temporary tool,
without adding project dependencies. A machine preparing this pinned tool can
run `GOBIN=/absolute/path/to/tools go install golang.org/x/vuln/cmd/govulncheck@v1.9.0`.
The gate installs locked npm development dependencies, requires browser/NATS
prerequisites, and writes a sanitized summary to `bin/release-verification.json`.
Local gate success does not resolve ownership/license/provenance approval.
Phase 5C adds `make test-tools` for the same pinned tools, automatic project-cache
resolution and separate fast/integration/security suites. The security/full
suites now require the scanner instead of silently omitting that check; see
[operations](PRODUCTION_OPERATIONS.md) and [current evidence](PHASE_5C_REPORT.md).
For the two local-replacement modules, external cache verification uses temporary
modfiles without the local SDK requirement; the SDK dependency graph is verified
independently. Local source is checked by inputs/tests, not a nonexistent public
module zip. Source go.mod/go.sum files are not rewritten by the gate.

## Security findings

The initial pinned `govulncheck v1.9.0` run reported six reachable findings in
`x/net v0.58.0` and `quic-go v0.59.0`: GO-2026-6617, GO-2026-6612, GO-2026-6611,
GO-2026-6610, GO-2026-6603 and GO-2026-5676. The smallest advisory-listed fixes
were applied: `x/net v0.60.0` and `quic-go v0.59.1`. Go minimum/toolchain guidance
and the Docker builder move to 1.27.2 because the HTTP/2 advisory also affects
earlier Go patches. See [HTTP/2 advisory](https://pkg.go.dev/vuln/GO-2026-6617)
and [QPACK advisory](https://pkg.go.dev/vuln/GO-2026-5676).

The follow-up root scan reports zero affected reachable symbols and zero affected
imported packages. It still reports one **module-only** advisory:
[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), the unmaintained
`golang.org/x/crypto/openpgp` package. SocketFabric does not import/call that
package; the upstream advisory has no fixed version. Do not describe this as
zero advisories in the full dependency graph. Do not add OpenPGP usage; review
again if dependency usage changes.

On 2026-10-16, npm audit returned zero reported vulnerabilities for all three
locked graphs (TypeScript SDK, external example tooling and browser runner).
The fallback current-source/artifact scan targets private keys, common provider
tokens, literal JWTs and known ENV credentials; it never prints matched values.
This is not a comprehensive secret audit. No usable Git history or installed
gitleaks/trufflehog scanner was available; history scanning remains NOT VERIFIED.
Container OS vulnerability scanning, third-party legal review, artifact signing
and authenticated source provenance remain NOT VERIFIED.

## Installation and verification

These artifacts are local preparation outputs, not publicly approved releases.
For a future approved release, first obtain the checksum list through the
owner-confirmed trusted distribution channel. SHA-256 alone does not prove
publisher authenticity.

```sh
# From the distribution directory on Linux:
sha256sum --check SHA256SUMS
# On macOS:
shasum -a 256 -c SHA256SUMS
```

Choose the matching OS/architecture. Linux example for a user-owned PATH:

```sh
mkdir -p "$HOME/.local/bin"
install -m 0755 cli/socketfabric-dev-linux-amd64 "$HOME/.local/bin/socketfabric"
# Add $HOME/.local/bin to PATH in your own shell profile if needed.
socketfabric version --json
socketfabric config validate
```

macOS uses the `darwin` artifact and the same user-owned installation approach.
On Windows, inspect `Get-FileHash -Algorithm SHA256` for the selected `.exe`,
compare with its entry in `SHA256SUMS`, copy it as `socketfabric.exe` into a
user-owned directory and add that directory to PATH. The Node verifier can also
check the complete manifest/checksum list. Cross-build success is not native
macOS/Windows execution, code signing, notarization or SmartScreen approval.
See the [CLI guide](CLI.md) for trusted ENV, status/ping/publish and CI exit codes.

## Versioning and compatibility proposal — owner decision required

No final public version is selected or bumped by Phase 4D.

| Component | Proposed strategy | Future tag / metadata |
| --- | --- | --- |
| Core + CLI | One SemVer line because they share the root Go module; binary metadata matches the reviewed root release | Root `vX.Y.Z` |
| Go SDK | Independent SemVer, permitting SDK fixes without forcing a gateway release | Nested module tag `sdk/go/vX.Y.Z` after canonical path approval |
| TypeScript SDK | Independent npm SemVer; publish only tested package/compatibility tuples | npm `X.Y.Z`, optional source tag `sdk/typescript/vX.Y.Z` |
| Examples/tests | Follow repository source revision; no independent release | Document the tested component tuple |

Go's module path/tag rules require subdirectory-prefixed tags and a `/v2` import
suffix when a Go module reaches major version 2. Apply those rules only after
the canonical repository path is verified. See the [Go module reference](https://go.dev/ref/mod#vcs-version).
Root `go install <canonical-root>/cmd/socketfabric@version` also requires public,
resolvable dependencies and removal of the current local replacement.

Compatibility means the existing `/v1/events`, `/ws` auth/membership frames,
event envelope, errors, project/room scoping and best-effort semantics remain
compatible. WebSocket frames currently have no negotiated protocol version;
do not invent support for one. The current working-tree gateway/SDK/CLI tuple is
the test target; no published cross-version matrix exists yet.

Recommended policy: additive compatible functionality increments minor; fixes
increment patch; stable incompatible APIs/protocols require a major release and
migration guide. Before 1.0, incompatible changes require a minor bump and an
explicit migration note; clients should pin tested ranges. Deprecate before
removal, retain an announced migration window in the same stable major, and
document any security exception. The owner must approve support windows and
the initial release tuple. Do not imply that matching major numbers alone prove
all SDK/gateway versions work together; record actual tested combinations.

## License decision

**BLOCKED.** No project LICENSE or npm SPDX license metadata is selected.
Consider [MIT](https://opensource.org/license/mit) for short permissive terms
with retained notices, or [Apache-2.0](https://www.apache.org/licenses/LICENSE-2.0)
for permissive terms that include an explicit contributor patent grant and
notice requirements. The owner must choose the terms and confirm copyright/
contribution authority. This preparation creates neither a project LICENSE nor
an implied license approval. Once approved, add the exact text and metadata to
the Go/npm/binary distribution and review third-party obligations.

`private: true` remains in the npm package; npm documents that flag as preventing
publication. See [npm package metadata](https://docs.npmjs.com/cli/v11/configuring-npm/package-json/#private).
Repository/homepage/bugs URLs and npm ownership must be verified before filling
them; a public 404 is not an ownership confirmation.

## First public release — manual steps after owner approval

1. Recover/clone the real Git history and verify owner, remote, branch, commit,
   clean source tree and repository visibility. Audit tracked files and full
   history with a reviewed scanner; resolve findings without exposing secrets.
2. Approve project license/contribution terms, npm scope membership, a monitored
   private security contact, component version strategy and the release tuple.
3. Replace provisional Go module/import paths everywhere with the verified
   canonical root and nested SDK path. Set approved npm license/repository/bugs
   metadata; update documented examples and compatibility assertions together.
4. Run the complete local quality gate on that source. Review checksums,
   dependency findings, notices, source revision and native platform coverage.
   Treat remaining BLOCKED/NOT VERIFIED items as explicit release decisions.
5. The owner creates/reviews the Go SDK's subdirectory-prefixed tag and pushes it
   only in the separately authorized publication step. This phase creates no tag.
6. In a clean external module with `GOWORK=off`, no replacements and empty
   dependency state, run `go get <canonical-sdk>@<approved-version>`, then build
   and exercise public SDK context/error/concurrency behavior. Record the result.
7. Pin that actual SDK version in root/examples, remove local replacements for
   public consumers and repeat `go install <canonical-root>/cmd/socketfabric@<approved-version>`
   from outside the repository. Verify the installed CLI against a real gateway.
8. Build a fresh approved distribution from the reviewed root revision. Review
   CHANGELOG, manifest/metadata, CLI checksums/notices, npm dry-pack/pack and clean
   external installation again. Apply an approved signing/provenance policy if
   selected; local checksums alone do not authenticate an owner.
9. Only after separate publication approval, the owner may finalize npm privacy/
   access metadata, publish the reviewed package, push the root tag and upload
   reviewed binaries. Do not include an unapproved license or invent registry
   identity to bypass a blocker. No step here executes those mutations.
10. Verify real registry/module installation and native supported-platform
    smoke tests, publish the compatibility matrix and update this report with
    actual public version/tag/URL evidence. Until then public install is NOT VERIFIED.

## Recommended Phase 5A

Design the smallest authenticated administration foundation around confirmed
operator needs and supported Core APIs: identity/roles, project scope, read-only
gateway readiness/diagnostics and redaction. First decide whether additional
admin APIs are justified and how to authorize them. Do not infer cluster-wide
presence/counts from one gateway. No dashboard, admin API, persistent store or
stronger delivery guarantee is implemented by this phase.

Phase 5A has subsequently added the optional management foundation; the paragraph
above records the Phase 4D recommendation, not the current feature matrix. See
[Management](MANAGEMENT.md) and its [implementation evidence](PHASE_5A_PLAN.md).
Go/npm identity, license and public-install blockers remain unchanged. No public
release is authorized or performed by the management implementation.
