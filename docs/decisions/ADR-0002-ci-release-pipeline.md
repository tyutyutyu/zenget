# ADR-0002: CI and release pipeline for zenget

- Status: Accepted
- Date: 2026-09-15

## Context

The public Go CLI has no GitHub Actions workflows or Git tags yet. Its root
module declares Go 1.24.1, and `internal/version/version.go` contains the
current version as a constant (`0.0.1`). The repository already requires
`go test ./...`, `go vet ./...`, and an 80% total coverage gate through
`scripts/check-coverage.sh`. `self-upgrade` reads releases from
`tyutyutyu/zenget`, accepts `v`-prefixed semantic versions, and supports
Linux and macOS. The release must therefore keep its tag, source version,
archive contents, and checksum asset consistent.

As of this decision's date, Go 1.27.1 and 1.26.8 are supported releases;
Go 1.24 is outside the Go project's two-release support window. Before the
first automated release, decide and document the minimum supported Go
version, with Go 1.26 recommended, and update `go.mod` in a reviewed change.
Build release binaries with a pinned, supported Go patch version rather than
an implicit `stable` toolchain. Preserve source compatibility only for Go
versions that CI actually tests.

## Decision

Use GitHub Actions for CI and a tag-triggered release, with GoReleaser v2
for the cross-platform archive and checksum matrix. Keep CI credentials
read-only and grant release publishing rights only to the publish job. A
protected `main` branch and protected `v*` tags enforce the review path.
Every action is pinned to a reviewed full commit SHA, with the associated
upstream tag recorded in a comment and updates proposed by Renovate or
Dependabot.

### CI

| Trigger | Required work |
| --- | --- |
| Pull request to `main` | Format, module integrity, lint, tests, coverage, race test, build, vulnerability scan, native Windows ownership tests, CodeQL. |
| Push to `main` | The same checks, so the protected branch remains auditable. |
| Weekly schedule / manual dispatch | Re-run vulnerability and CodeQL scans against newly published findings. |
| Version tag | Re-run required checks on the exact tagged commit before release assembly. |

The Go jobs use `go.mod` for the minimum language version and explicitly
install the supported patch versions in the test matrix (initially Go
1.26.8 and 1.27.1 after the minimum-version change). Cache only Go module
downloads/build data. Set `GOTOOLCHAIN=local` in the release job so it cannot
silently download a different compiler. The default workflow token has
`contents: read`; the CodeQL upload job receives `security-events: write`
only where GitHub permits it. Fork pull requests receive no write token or
secrets. Verify CodeQL's fork-PR check behavior before requiring that check.

Required CI checks:

1. Verify tracked Go files are `gofmt`-clean; run `go mod verify` and reject
   an uncommitted `go mod tidy -diff`. Do not mutate the checkout in CI.
2. Run `go vet ./...` and a pinned golangci-lint v2 configuration enabling
   `staticcheck`, `errcheck`, `ineffassign`, and `unused`. Review current
   findings before making the lint job required; fix the baseline or narrowly
   document a specific exclusion. Avoid a broad repository-wide ignore.
3. Run `scripts/check-coverage.sh` as the existing 80% gate and
   `go test -race ./...` on Linux/amd64. Build with `go build ./...`.
4. Run `govulncheck ./...` with a pinned tool version against the Go
   vulnerability database. A reachable known vulnerability blocks release;
   triage scanner or database outages as an explicit CI failure.
5. Run CodeQL Go analysis in a separate, SHA-pinned workflow on PR, `main`,
   and a weekly schedule. Start with the default query suite and make its
   completed analysis a required check. Keep its findings in GitHub code
   scanning for review and remediation.

The nested `tools/attestation-harness` module is a spike, not production CLI
code. Its offline tests can run in a separate, non-release CI job with a
pinned toolchain and `GOPROXY=off` after dependencies are cached; it must not
expand the root module or alter release inputs.

### Minimum Go version decision

The minimum supported Go version for the root `zenget` module is Go 1.26.0.
The compatibility matrix tests the latest available patch releases in the
supported window: Go 1.26.8 and Go 1.27.1. Release assembly uses Go 1.26.8
with `GOTOOLCHAIN=local`, so the release cannot silently download another
compiler. The `go.mod` directive is therefore updated from Go 1.24.1 to
`go 1.26.0`; source compatibility below Go 1.26 is not promised.

This decision applies only to the root production module. The nested
`tools/attestation-harness` module remains a separate spike and does not
change the runtime attestation decision recorded in Backlog decision-0001.

### Release

1. Prepare a version-change pull request: update the `Version` constant,
   changelog/release notes, and any compatibility documentation. Merge only
   after all required checks pass. Create a signed, annotated `vX.Y.Z` tag
   on the merged `main` commit. No release is created from a branch head,
   moving tag, or arbitrary workflow dispatch input.
2. On tag push, verify strict SemVer syntax, `v` plus the source `Version`
   constant, tag ancestry on `main`, and a unique tag/commit association. Run
   `git verify-tag --raw` and require its GPG fingerprint to match the reviewed
   `ZENGET_TRUSTED_GPG_FINGERPRINTS` repository variable containing exact full
   fingerprints; a text `gpgsig` field
   alone is not evidence. Unknown, malformed, or unsupported signatures fail
   closed. Re-run CI on that exact SHA. The release assembly job checks out
   that SHA and uses the pinned Go compiler and GoReleaser version.
3. Run `goreleaser check` in PR CI and a snapshot build to validate the
   archive configuration. For the tagged build, generate `CGO_ENABLED=0`
   binaries for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64.
   Publish a single `zenget` executable per `.tar.gz` or `.zip` archive,
   named with version, OS, and architecture. Set `-trimpath` and a stable
   archive timestamp. Generate one SHA-256 `checksums.txt` asset covering
   every archive; its names must match the assets exactly.
4. Before publishing, extract each archive and check its single executable,
   platform name, executable bit where relevant, and `zenget --help` on
   runnable host platforms. The build artifact is checksum-verified and run
   natively on Linux amd64, macOS arm64, and Windows amd64 before attestation;
   no smoke job rebuilds or downloads a public release. Check every archive
   against `checksums.txt` and verify the tagged source version. Attach a GitHub build-provenance
   attestation to each final archive and the checksum file, with the
   `id-token: write` and `attestations: write` rights scoped to this job.
   Optionally attach an SPDX SBOM after its format and maintenance cost are
   reviewed.
5. Transfer the verified, attested artifacts to the publish job and verify
   their SHA-256 values again after transfer. Upload them to a **draft**
   GitHub release. The publish job alone receives `contents: write`. A
   maintainer reviews
   the draft asset list, checksums, notes, and provenance before publishing
   it. Publishing is the explicit promotion point. Preserve the tag and
   assets as immutable after promotion; corrections use a new patch version.
6. After publication, run a public download smoke test for checksum
   verification and Linux/macOS `self-upgrade` from an older released
   version. Windows gets extraction/help smoke testing; in-place Windows
   self-upgrade is intentionally unsupported.

Release verification and attestations describe *zenget's own published
artifacts*. They do not change zenget's runtime verification of releases it
installs. Backlog decision-0001 deferred that separate production feature.

### Protection and failure handling

Require pull requests and the CI/CodeQL status checks for `main`; forbid
force pushes. Protect `v*` tags against rewrite/deletion and restrict their
creation to maintainers. Use a release concurrency group per tag so retries
cannot publish two versions. Do not use `pull_request_target` to build
untrusted PR code. Do not pass personal access tokens or long-lived signing
keys into the normal CI path; the release uses the scoped GitHub token and
OIDC. If a pre-publish check fails, leave the release as draft and do not
promote it. If a published binary is defective, publish a corrected patch
release and mark the affected release clearly; never replace an asset under
the same version.

## Alternatives considered

- A single workflow that builds and publishes immediately on tag push is
  simpler, but it grants publishing rights to the build and removes the
  reviewable draft gate.
- Hand-written `go build` loops avoid a release dependency, but require
  maintaining archive, checksum, and platform naming logic that GoReleaser
  already provides.
- A broad lint set or `gosec` as an immediate hard gate may produce noisy
  findings in this existing codebase. Start with focused correctness checks,
  `govulncheck`, and CodeQL; add further linters when their baseline is clean.
- Automatically bumping versions or cutting tags from CI obscures the
  reviewed source/version relationship. Use a reviewed version PR and an
  explicit maintainer tag.

## Consequences and rollout

The first implementation PR adds the CI workflow and lint configuration,
then establishes a clean required-check baseline. A second focused PR adds
the GoReleaser configuration and draft release workflow. The minimum-Go
compatibility decision must precede both. Validate workflows with a PR
snapshot and a disposable prerelease tag before the first stable publication.
Enabling branch/tag protection and publishing a draft are repository settings
and release actions, respectively, to perform only after the configuration
is reviewable.

The weekly `Weekly security scan` workflow runs the pinned vulnerability check
on the default branch every Monday and through manual dispatch. Scanner or
database errors fail the job. The extra scans and race test increase CI time,
and a public-release smoke test depends on GitHub availability. Failed network
scans should surface as failures with a manual rerun path, not disappear behind
`continue-on-error`.
Attestations prove the build identity and artifact digest, not the absence
of vulnerabilities; the static checks and release review remain separate.

## Sources checked on 2026-09-15

- Go release policy and supported versions: https://go.dev/doc/devel/release
- Go vulnerability scanning: https://go.dev/doc/security/vuln/
- Staticcheck behavior: https://staticcheck.dev/docs/running-staticcheck/cli/
- golangci-lint v2 configuration: https://golangci-lint.run/docs/configuration/file/
- GitHub Actions secure use: https://docs.github.com/en/actions/reference/security/secure-use
- GitHub CodeQL setup: https://docs.github.com/en/code-security/concepts/code-scanning/setup-types
- GitHub artifact attestations: https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations
- GoReleaser Go builds and checksums: https://goreleaser.com/customization/builds/builders/go/ and https://goreleaser.com/customization/package/checksum/
