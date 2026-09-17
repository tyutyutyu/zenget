# Releasing zenget

The release process is deliberately split into a reviewed source change, an
immutable signed tag, a verified draft, and a separate maintainer publication.
The root module supports Go 1.26.0 and newer. CI tests Go 1.26.8 and 1.27.1;
release builds use Go 1.26.8 with `GOTOOLCHAIN=local`.

## Prepare and tag a release

1. Update `internal/version/version.go`'s `Version` constant, release notes,
   and compatibility documentation in a pull request.
2. Merge the pull request into `main` after the required checks pass.
3. From the merged `main` commit, create and push a signed, annotated tag:

   ```sh
   git switch main
   git pull --ff-only origin main
   git tag -s -a vX.Y.Z -m "zenget vX.Y.Z"
   git push origin vX.Y.Z
   ```

   Set the repository variable `ZENGET_TRUSTED_GPG_FINGERPRINTS` to the
   reviewed maintainer full GPG fingerprint(s) before pushing the tag. Set
   `ZENGET_TRUSTED_GPG_PUBLIC_KEYS` to the corresponding armored public keys
   exported with `gpg --armor --export FULL_FINGERPRINT`. Review both variables
   together during initial setup and rotation; never export a private key.
   The verifier imports these public keys into a temporary isolated keyring,
   so fresh runners need neither preinstalled keys nor a network keyserver.
   Missing or invalid public-key input fails closed. The release
   workflow runs `git verify-tag --raw` and accepts only a cryptographically
   valid annotated GPG tag whose signer matches that variable. Missing,
   malformed, SSH/X.509, or unknown signatures fail closed; no private key is
   stored in GitHub. Rotate the public fingerprint variable through a reviewed
   repository-settings change.

   The release workflow rejects non-`vX.Y.Z` tags, lightweight or unsigned
   tags, tags outside `origin/main`, tags pointing at a different event commit,
   duplicate tags on the same commit, and tags that do not match `Version`.

## What the workflows do

The `CI` workflow runs on pull requests and pushes to `main` and is also
reusable by the tag workflow. Each Go matrix job runs formatting, module
integrity, `go vet`, tests, the 80% coverage gate, race tests, and a build.
The focused lint job enables only `staticcheck`, `errcheck`, `ineffassign`,
and `unused`; the vulnerability job runs pinned `govulncheck`. The
`Windows ownership tests` job runs the ACL-backed fileowner, registry,
recipe-policy, and trust tests on `windows-latest`; the `release-config` job
validates GoReleaser and smoke-tests a snapshot.

`CodeQL` is a separate SHA-pinned workflow on pull requests, `main`, and a
weekly schedule. It uses `pull_request`, never `pull_request_target`, so fork
code is not granted write-capable secrets. GitHub may downgrade
`security-events: write` for fork pull requests; if GitHub cannot upload that
analysis, the check remains visible as a failure and must be rerun from an
eligible context.

The `Weekly security scan` workflow runs the same pinned `govulncheck` on the
default branch every Monday and exposes a manual dispatch. Advisory, database,
or scanner failures remain failed checks and are never hidden by
`continue-on-error`.

On a valid version tag, `Release` reruns CI and CodeQL on the exact tagged
source, builds these five archives with GoReleaser, and verifies them before
the publishing step:

| Archive | Target |
| --- | --- |
| `zenget_X.Y.Z_linux_amd64.tar.gz` | Linux amd64 |
| `zenget_X.Y.Z_linux_arm64.tar.gz` | Linux arm64 |
| `zenget_X.Y.Z_darwin_amd64.tar.gz` | macOS amd64 |
| `zenget_X.Y.Z_darwin_arm64.tar.gz` | macOS arm64 |
| `zenget_X.Y.Z_windows_amd64.zip` | Windows amd64 |

`checksums.txt` is a single SHA-256 manifest covering exactly those files.
The archive verifier checks the manifest, executable count, root-level names,
Unix executable bits, and Linux amd64 `zenget --help`. The verified files are
then checksum-verified and run natively on Linux amd64, macOS arm64, and
Windows amd64 before attestation. A later job rechecks the checksums after
artifact transfer and creates or updates a **draft** release. Only that
publisher job has `contents: write`; the workflow never promotes a release.

The maintainer reviews the draft asset list, checksums, generated notes, and
provenance in GitHub, then publishes it manually. A published version is
immutable: corrections require a new patch version.

## Post-publication smoke

After publishing, manually run the `Public release smoke` workflow with the
published tag. It downloads the public checksum manifest and every archive
using HTTPS-only transfers and HTTPS-only redirects; those transport
restrictions work together with checksum verification. It checks all five
hashes and archive layouts, runs Linux amd64 and macOS arm64
help smoke, and exercises Windows amd64 extraction/help. Supplying
`previous_tag` additionally downloads the older Linux/macOS release and runs
`zenget self-upgrade` in an isolated config directory. If no previous release
exists, the workflow emits an explicit notice and records that only checksum
and help smoke ran. Windows in-place self-upgrade remains intentionally
unsupported.

The same checks are available locally for a downloaded GoReleaser `dist/`:

```sh
scripts/verify-release-artifacts.sh dist --run-help
```

## Repository protections

Enable repository-owned, active protections before the first stable release:

- `main`: pull request review, stale-review dismissal, conversation
  resolution, no force-push or deletion, and required checks named
  `Go quality / 1.26.8`, `Go quality / 1.27.1`, `Focused static lint`,
  `Go vulnerability scan`, `Windows ownership tests`,
  `Release configuration snapshot`, and `CodeQL`;
- `refs/tags/v*`: no creation except maintainers, no update/force-push, and no
  deletion.

The `tyutyutyu/zenget` repository is public. Visibility alone does not enable
required reviews, checks, or immutable tag rules, so the repository-owned
rulesets must be inspected and enabled separately. If the GitHub account lacks
the administration permission or plan needed for those rulesets, the release
workflow remains an explicit source/tag and artifact gate and the missing
settings stay a visible release prerequisite.

Windows configuration files use an ACL-backed ownership seam. The owner SID
must match the current token, reparse points and NULL DACLs are rejected, and
write access granted to untrusted principals fails closed. New registry,
recipe-policy, trust, and cache paths receive an owner/SYSTEM/Administrators
ACL before they are populated; Unix continues to enforce uid and mode bits.

Release provenance is provenance for zenget's own published files. It does not
enable runtime attestation verification for releases installed by zenget;
Backlog decision-0001 deliberately defers that separate production feature.
