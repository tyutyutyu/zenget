# zenget

`zenget` installs single-binary GitHub releases into `~/.local/bin` and keeps track of what it installed, so binaries can be upgraded and cleanly removed later.

## Installation

Requires Go 1.24+.

```sh
go build -o zenget .
```

Copy the resulting `zenget` binary somewhere on your `PATH`.

## Usage

Install the latest release of a GitHub repository:

```sh
zenget install org/repo
```

### Short command aliases

zenget supports only the following explicit, lowercase aliases. Full command
names remain available, and aliases preserve the same arguments, flags,
validation, output, and exit status as their full commands.

| Command | Aliases |
| --- | --- |
| `install` | `i` |
| `uninstall` | `rm`, `un` |
| `upgrade` | `up` |
| `list` | `ls`, `l` |
| `config` | `cfg` |
| `registry` | `reg` |
| `project` | `proj` |
| `config list` | `ls`, `l` |
| `registry list` | `ls`, `l` |
| `cache list` | `ls`, `l` |
| `policy recipe list` | `ls`, `l` |
| `project trust list` | `ls`, `l` |
| `registry remove` | `rm` |

For example:

```sh
zenget i org/repo --tag v1.2.3
zenget rm org/repo
zenget up --dry-run
zenget ls --json
zenget cfg get checksum_policy
zenget reg ls --json
zenget proj trust ls --json
zenget reg rm local
```

Aliases are exact and case-sensitive: names such as `inst`, `u`, `c`, `r`,
`p`, and `I` are not aliases. The existing `list -u` option remains the
short form of `--updates`.

If a release has multiple assets, zenget tries to pick the one matching your
platform and prefers `.tar.gz`/`.tgz` over `.zip` over a raw binary. You can
override the selection explicitly with `--asset`:

```sh
zenget install org/repo --asset asset-name.tar.gz
```

For release filenames that change their embedded version, use one reusable
selector instead of saving a concrete filename. `--asset-match` is a
case-insensitive literal substring; `--asset-match-regex` uses Go's RE2
regular-expression syntax. They are mutually exclusive with each other and
with `--asset`:

```sh
zenget install org/repo --asset-match linux-amd64
zenget install org/repo --asset-match-regex '^repo-linux-amd64-v[0-9.]+\.(tar\.gz|zip)$'
```

The selector runs after checksum/signature/source/noise filtering, then the
normal platform, libc, archive-format, and deterministic name rules choose
one matching asset. A successful selector install stores a typed
`asset_selector` object (`version`, `type`, and `pattern`) in the per-app
configuration, so a later release such as `repo-linux-amd64-v2.tar.gz` can be
selected without rewriting the rule. A concrete `--asset` choice remains a
legacy exact/unique-substring choice and takes precedence over selectors.

### Release series filters

When a repository publishes several release series, restrict the release tag
candidates with a `path.Match` glob:

```sh
zenget install acme/tool --release-filter 'cli-*'
```

The glob supports `*`, `?`, and character classes such as `[12]`; `*` does not
cross a `/` in a tag. The selected filter is stored in the application state,
so later flag-free installs, `upgrade`, and `list --updates` continue to use
the same release series. An exact `--tag` still takes precedence. Pass an
explicit empty value (`--release-filter=`) to clear the saved filter.

### Versioned local install recipes

Use `--recipe` when a repository has a stable release naming convention that
should be shared as a local, versioned rule:

```sh
zenget install acme/widget --recipe ./recipes/acme-widget.json
```

The recipe must be an explicitly named local regular, non-symlink JSON file.
It is never downloaded, discovered automatically, read from stdin, or allowed
to run commands, scripts, hooks, or arbitrary URLs. The minimal schema version 1
recipe is:

```json
{
  "schema_version": 1,
  "repository": "acme/widget"
}
```

Optional fields make the release convention deterministic:

```json
{
  "schema_version": 1,
  "repository": "acme/widget",
  "supported_systems": ["linux/amd64", "darwin/arm64"],
  "asset_selector": {
    "version": 1,
    "type": "regex",
    "pattern": "^widget-(linux-amd64|darwin-arm64)-v[0-9.]+\\.tar\\.gz$"
  },
  "archive_binary": "bin/widget",
  "target_name": "widget",
  "checksum": {
    "type": "regex",
    "pattern": "^widget-(linux-amd64|darwin-arm64)-v[0-9.]+\\.sha256$",
    "format": "sha256sum"
  }
}
```

`supported_systems` is an allowlist for the effective `os/arch`; `--system`
selects that target platform. `asset_selector` uses the same case-insensitive
substring or Go RE2 regex representation as `--asset-match` and
`--asset-match-regex`. `archive_binary` is either an archive filename or a
safe relative archive path using `/`, and `target_name` is the installed
single-component command name. A recipe checksum selector must match exactly
one asset in the current release. `format` is either `raw` (one bare SHA-256
hex digest) or `sha256sum` (a checksum line naming the selected release asset);
the checksum asset is downloaded through the normal authenticated provider
path and verified before installation.

The public Draft 2020-12 JSON Schema for recipes is available at
`schemas/recipe-v1.schema.json`; editors and registry CI can use it for
completion and validation, while zenget enforces the same contract with its
strict local decoder.

The effective precedence is explicit CLI choice, saved per-application choice,
recipe rule, then automatic selection. A recipe is not stored in state or app
configuration; only the resolved compatible choices may be saved. Omitting the
recipe checksum preserves zenget's built-in sidecar/aggregate checksum checks.

Recipes describe selection and verification rules, but they are not lockfiles
and do not pin an exact release artifact or provide artifact provenance. Use
the manifest and `lock`/`apply --lock` workflow when exact release metadata and
downloaded content hashes must be recorded.

### Remote recipe-source policy

Remote recipe registries are deny-by-default. The policy is stored at
`${XDG_CONFIG_HOME:-~/.config}/zenget/recipe-policy.json`; a missing or empty
policy denies every remote recipe, as do an unknown schema, an unknown field,
or an unsafe policy file. The file must be a regular, non-symlink file owned by
the current user with permissions no more permissive than `0600`. zenget never
silently repairs an unsafe or malformed policy.

Add or remove an exact GitHub source with an optional repository-relative path
prefix:

```sh
zenget policy recipe allow github:acme/widget
zenget policy recipe allow github:acme/widget --path-prefix recipes
zenget policy recipe deny github:acme/widget --path-prefix recipes
zenget policy recipe list
zenget policy recipe list --json
zenget policy recipe check github:acme/widget/recipes/v1/recipe.json
```

An allow rule contains only the exact `github.com` host, case-sensitive GitHub
`owner` and `repository`, and an optional literal `path_prefix`. A prefix
matches itself and descendants, with a path-component boundary; it does not
match a similarly named sibling such as `recipes-old`. Wildcards, regular
expressions, URL globs, branches, package-name patterns, absolute paths,
dot-dot segments, query strings, fragments, and percent-encoded spellings are
rejected. `allow`, `deny`, and `list` use deterministic ordering and repeated
operations are idempotent. `check` is read-only and reports the decision,
matching rule, and reason in human-readable or JSON form.

This policy is a source trust boundary, not artifact provenance: an allowed
repository/path is permitted to provide a recipe, but that decision does not
prove who authored the recipe or where an installed artifact came from. The
explicit local `install --recipe ./file.json` workflow remains available and
is not subject to this remote-source allowlist; its local-file and strict
schema protections still apply.

### Commit-pinned remote recipe registries

Use a named registry when the same strict recipe rules should be shared while
remaining reproducible. A registry source must be exactly:

```text
github:owner/repository@<40-hex-commit>:<relative-root>
```

The commit is case-normalized to lowercase and cannot be a branch, tag,
`HEAD`, short SHA, URL, query, fragment, absolute path, or path containing a
dot-dot segment. An empty root after the final colon means the repository
root. Adding or removing a registry changes only
`${XDG_CONFIG_HOME:-~/.config}/zenget/registries.json`; it does not contact
GitHub:

```sh
zenget registry add stable github:acme/recipe-registry@0123456789abcdef0123456789abcdef01234567:recipes
zenget registry list
zenget registry list --json
zenget registry remove stable
```

Remote registries remain deny-by-default. Before syncing, allow the registry
index and recipe paths with the remote recipe policy, for example:

```sh
zenget policy recipe allow github:acme/recipe-registry --path-prefix recipes
zenget registry sync stable
zenget registry sync --all
```

The pinned source's `<relative-root>/registry.json` must be a strict schema
version 1 object:

```json
{
  "schema_version": 1,
  "entries": [
    {"repository": "acme/widget", "path": "widget.json", "sha256": "<64 lowercase hex>"}
  ]
}
```

Entries are sorted by repository and then path. Each repository and path can
occur only once; paths are relative to the configured root and every recipe's
own `repository` must match the index. Unknown fields, duplicate JSON keys,
bad ordering, invalid paths/digests, and schema or hash mismatches stop the
sync before a new snapshot is published. The snapshot is stored below
`${XDG_CACHE_HOME:-~/.cache}/zenget/registries` and is addressed by the full
source and index digest. Credentials, authorization headers, URLs, and local
recipe paths are never stored in the registry config, index, cache, or install
state.

Select one synchronized snapshot explicitly at install time:

```sh
zenget install acme/widget --registry stable
zenget registry sync stable --offline
zenget install acme/widget --registry stable --offline --tag v1.2.3
```

`install --registry` never searches another registry and never falls back to a
local recipe or to heuristics when the named snapshot is missing, denied, or
corrupt; those states are hard errors. `--offline` makes no network request: it
requires a valid policy-authorized registry snapshot and a verified artifact
already present in zenget's release artifact cache. A cache miss fails closed.
The local `install --recipe ./file.json` workflow is separate, is not a
registry selector, and is not eligible for this offline snapshot workflow.

A plain `zenget install org/repo` with no recipe selector also consults the
synchronized snapshots, in registry configuration order, before heuristics run:

```sh
zenget install acme/widget
zenget install acme/widget --no-registry
```

When an index contains the repository, its recipe supplies the asset selector,
archive binary, and target name for that install (reported as
`Using recipe for acme/widget from registry stable (...)`), subject to the
same repository-match and supported-systems validation as every other recipe
source. When no registry is configured, no snapshot is synchronized, the
recipe-source policy denies a source, a snapshot is corrupt, or no index lists
the repository, the lookup is skipped silently and the install proceeds with
the normal heuristic resolution exactly as if no registry existed — there is
no failure mode in which automatic resolution blocks a non-registry install.
`install --no-registry` disables only this automatic lookup; explicit
`--registry` and `--recipe` are unaffected. Automatic resolution reads only
the local snapshot cache, so it adds no network requests; `zenget registry
sync` remains the only network step, and `--offline` keeps its existing
meaning of requiring `--registry`.

Enabling a registry is a deliberate three-step trust decision: allow the
source in the remote recipe policy, add the named source, and synchronize a
snapshot. The recipe supplied by a registry is data from a commit-pinned
remote source, not artifact provenance: the policy gates which sources may be
read, the snapshot format binds recipes to index SHA-256 digests, and
deny-by-default remains the standing state for unlisted sources. Registry
recipes must satisfy the same strict schema version 1 contract, supported
systems matrix, and checksum policy as a local `--recipe` file, and they never
override explicit CLI flags or saved per-application choices.

The pilot registry content lives in a separate repository
(`../zenget-registry` as a local checkout). It is a local pilot only:
decision-0002 records a production registry launch as NO-GO until the
operational go conditions (dedicated repository governance, review ownership,
revocation channel, and CI matrix coverage) are accepted and reproducibly met;
no remote registry endpoint is published yet.

### Global checksum policy

The global checksum policy controls normal release-asset verification:

```sh
zenget config set checksum_policy required
zenget install acme/widget --checksum-policy if-present
zenget config get checksum_policy
```

The only values are `if-present` and `required`. A missing configuration field
defaults to `if-present`; there is no `off` mode. `if-present` permits an asset
only when no checksum source is published for it. A discovered malformed,
wrong-target, or mismatching checksum is always an error. `required` also
rejects an asset with no unambiguous SHA-256 source, before extraction or
persistent state, app-configuration, wrapper, or artifact changes. The
explicit `install --checksum-policy` value takes precedence over the global
configuration, which takes precedence over the default.

The policy is shared by direct installs, implicit upgrades, `ensure` restores,
and manifest `apply` installs/restores. Upgrade and apply dry-runs report the
effective policy and selected repository, release tag, and asset. A recipe's
explicit checksum remains mandatory regardless of this policy, and
`apply --lock` continues to require its recorded archive SHA-256 hash.

When the asset or an archive contains more than one plausible executable,
zenget asks for a numbered choice in an interactive terminal. Use
`--asset`, `--bin-name`, and `--name` to make the same choices in scripts:

```sh
zenget install org/repo --asset repo-linux-amd64.tar.gz \
  --bin-name bin/repo --name repo
```

The selected values are saved per repository in
`${XDG_CONFIG_HOME:-~/.config}/zenget/apps/<org>__<repo>.json`. Saved choices
are checked on later upgrades. If a saved asset or archive member is no longer
available, an interactive install asks for that choice again; a non-interactive
install fails with the corresponding flag hint.

Install a specific release tag with `--tag`. An exact tag match is tried first;
if no exact match exists, the most recent release whose tag contains the given
string is installed:

```sh
zenget install org/repo --tag v1.2.3
zenget install org/repo --tag v1.2
```

Pin an installed application to prevent implicit upgrades. An explicit
`--tag` still installs the requested release while preserving the pin. Use
`unpin` to allow normal upgrades again:

```sh
zenget pin org/repo
zenget install org/repo             # reports the pinned version and exits
zenget install org/repo --tag v2.0.0 # explicit install; pin remains
zenget unpin org/repo
```

To avoid installing a release immediately after it is published, require a
minimum age with `--min-age-days`. The newest stable release that is at least
that many days old is selected; draft and prerelease releases are ignored.
This flag cannot be combined with `--tag`, and it uses the first page returned
by GitHub's releases API:

```sh
zenget install org/repo --min-age-days 7
```

Preview the same release and asset decision without installing or downloading
anything:

```sh
zenget inspect org/repo
zenget inspect org/repo --tag v1.2 --system linux/amd64 --json
zenget inspect org/repo --asset repo-linux-amd64.tar.gz
```

`inspect` accepts `--tag`, `--min-age-days`, `--system`, `--asset`,
`--asset-match`, and `--asset-match-regex` with
the same validation and release-selection rules as `install`. Explicit
`--asset` wins over the saved choice in
`${XDG_CONFIG_HOME:-~/.config}/zenget/apps/<org>__<repo>.json`; a saved asset
is read and reported as `persisted`, but is never repaired or rewritten by
`inspect`. The report includes the repository, selected tag, publication time
and age, effective OS/architecture/libc, selected asset and format, selection
source, and every release asset with a deterministic disposition and reason.

`--json` emits a stable, ANSI-free object with `repository`, `release`,
`target`, `status`, nullable `selected_asset`, `selection_source`, and sorted
`assets` fields. Checksum evidence is metadata-only: a provider digest or the
names of matching sidecar/aggregate checksum assets are shown; checksum,
signature, source, or binary content is never downloaded. A successful
selection exits 0. Empty, ambiguous, incompatible, no-match, or stale saved
choices are reported completely and exit non-zero; API and argument errors
also exit non-zero.

The additive `selector` JSON object (and its human-readable line) reports the
selector source (`automatic`, `explicit`, or `persisted`), type (`asset`,
`substring`, or `regex`), pattern, excluded asset names, and final decision.

Public release metadata can be inspected directly:

```sh
zenget inspect cli/cli --json
```

Private release metadata uses the same optional token as `install`; the token
is sent only as an API request header and is never included in the report:

```sh
ZENGET_GITHUB_TOKEN="$GITHUB_TOKEN" zenget inspect acme/private-tool
```

Upgrade all installed, unpinned applications or only the repositories named on
the command line. Release lookups are bounded and parallel, while installs run
in deterministic repository order:

```sh
zenget upgrade
zenget upgrade acme/widget --yes
zenget upgrade --dry-run
```

The command prints a plan first. A dry run resolves releases and validates the
saved asset, target, and platform choices without downloading or changing
files. Interactive runs ask for one confirmation; scripts must pass `--yes`.
Pinned applications are reported as `skipped`. The final summary reports
`updated`, `unchanged`, `skipped`, and `failed`; a failed application does not
prevent the remaining applications from being attempted, but returns a
non-zero exit status.

### Update zenget itself

The `self-upgrade` command checks the latest release from zenget's own
repository and, when a newer semantic version is available, replaces the
running binary atomically:

```sh
zenget self-upgrade
```

The repository coordinate is the single `Repository` constant in
`internal/version/version.go` (`tyutyutyu/zenget`). The command accepts `v` or
`V` prefixes and semantic-version prerelease tags, verifies available SHA-256
checksums, and supports the same host-platform asset formats as `install`.
It resolves symlinks before replacing the target, so a symlink used to launch
zenget remains intact.

Self-upgrade is supported on Linux and macOS. Windows executable replacement
while the process is running is intentionally unsupported; replace
`zenget.exe` manually or use a package manager. The command never writes the
zenget installation state. If the running binary is a zenget-managed artifact
or wrapper pair, it refuses to replace it and reports that the regular
`upgrade` command must be used instead, preserving the recorded artifact hash
and rollback state.

Create and edit a portable manifest without installing anything. `init` writes
an empty schema-version 1 manifest to `zenget.json` in the current directory;
use `--output` for another local path and `--force` to replace an existing
regular file. Symlink targets are always rejected:

```sh
zenget init
zenget init --output team-zenget.json
zenget init --output team-zenget.json --force
```

Use `add` to resolve one release and record its exact tag and selected asset in
an existing manifest. Without `--tag`, the latest release is queried and the
returned tag is written verbatim. `--asset`, `--bin-name`, `--name`, and
`--system` record the same choices accepted by `install`; `add` only updates
the manifest and does not touch installed state, app configuration, wrappers,
or downloaded files. An existing repository entry requires `--replace`:

```sh
zenget add acme/widget --tag v1.2.3 --asset widget-linux-amd64.tar.gz
zenget add acme/widget --replace --system linux/amd64 \
  --bin-name bin/widget --name widget
```

The generated manifest includes the public Draft 2020-12 schema reference at
`https://raw.githubusercontent.com/tyutyutyu/zenget/main/schemas/manifest-v1.schema.json`.
Editors can use that schema for completion and validation; zenget validates
the same contract locally and never downloads the schema at runtime.

Commands that operate on a project manifest (`add` and `apply`) discover the
nearest regular, non-symlink `zenget.json` by walking from the current working
directory toward the filesystem root. The selection precedence is an explicit
`--manifest` path, `ZENGET_MANIFEST`, then automatic discovery. Relative
overrides are resolved from the process working directory. Use
`--no-manifest-discovery` or `ZENGET_NO_MANIFEST_DISCOVERY=1` to disable the
last step; explicit paths still work. `zenget manifest path` reports the
canonical path, source, and schema version, in human-readable or `--json`
format, and exits with status 2 when no manifest is found:

```sh
zenget manifest path
zenget manifest path --json
zenget apply                 # discover zenget.json
zenget apply --manifest ./zenget.json
zenget add acme/widget --manifest ./zenget.json
```

`init` is intentionally different: it always creates `zenget.json` in the
current working directory (or the path given by `--output`) and never searches
parent directories.

### Trusted project activation and lazy installation

Project manifests are inert until their exact local path is trusted. Trusting a
manifest checks that it is a regular, non-symlink file owned by the current
user and that its path is not group- or world-writable. The project root and
manifest path are stored canonically; a later ownership, permission, or
symlink change invalidates the trust.

Trust and activate a project without downloading anything:

```sh
zenget project trust --manifest ./zenget.json
zenget project trust list
zenget project activate --manifest ./zenget.json
```

`project activate` validates the complete manifest before changing any managed
project shims. It rejects duplicate target names and foreign files, and does
not change the global install registry or active application state. Use
`zenget project untrust --manifest ./zenget.json` to remove the trust record;
`zenget project trust list --json` prints the canonical records.

Lazy installation is an additional opt-in. Enable it explicitly, then use the
activated project shims:

```sh
zenget config set lazy_install true
zenget project trust --manifest ./zenget.json
zenget project activate --manifest ./zenget.json
```

When a trusted project target is missing from the artifact cache, zenget fetches
only the manifest's exact GitHub release tag and exact selected asset, verifies
it with the effective checksum policy, enforces the configured resource limits,
and atomically caches the extracted binary. Concurrent first starts share one
download. Lazy execution never prompts, falls back to a global installation, or
writes the global registry; it preserves the target's arguments, environment,
exit status, and signal behavior. Set `ZENGET_LAZY_INSTALL=0` for a temporary
runtime opt-out. The default is disabled, and a project must remain trusted and
safe at both activation and lazy execution time.

Export the installed applications' portable installation choices as a
schema-versioned manifest. By default it is written to stdout; `--output`
writes an atomic local file and refuses to replace an existing file unless
`--force` is supplied:

```sh
zenget export
zenget export --output zenget-manifest.json
zenget export --output zenget-manifest.json --force
```

Apply accepts only a local manifest path. It resolves every recorded release
tag exactly, selects the recorded asset and archive member, and leaves local
applications absent from the manifest untouched. `--dry-run` resolves releases
and prints the sorted `install`, `restore`, `unchanged`, and `failed` plan
without downloading or changing state, app configuration, wrappers, or
artifacts:

```sh
zenget apply zenget-manifest.json --dry-run
zenget apply zenget-manifest.json
```

Create a strict artifact lock from a manifest when reproducible release
metadata and archive content are required. The lock resolves each recorded
tag and exact asset, records the provider asset ID/size and SHA-256 values, and
is written atomically without overwriting an existing file unless `--force` is
used. It contains no download URL, token, local path, or timestamp:

```sh
zenget lock zenget-manifest.json --output zenget-lock.json
zenget lock zenget-manifest.json --output zenget-lock.json --force
```

Apply a lock with `--lock`. Before any installation mutation, zenget resolves
the same release and provider asset, downloads every selected artifact to
staging, and checks its recorded size and SHA-256. A mismatch aborts the whole
locked content preflight. `--dry-run --lock` performs metadata checks only and
reports content verification as pending:

```sh
zenget apply zenget-manifest.json --lock zenget-lock.json
zenget apply zenget-manifest.json --lock zenget-lock.json --dry-run
```

### Renovate manifest updates

The repository ships a shareable Renovate preset at
[`renovate/zenget.json`](renovate/zenget.json). It finds each
`repository`/`provider`/`tag` sequence in a canonical schema-version 1
`zenget.json`, queries the `github-releases` datasource, and updates only the
exact release tag. The preset uses `loose` versioning so common `v`-prefixed
SemVer tags and documented non-standard GitHub release tags remain usable.

Pin the preset to a zenget release in the consuming repository:

```json
{
  "extends": [
    "github>tyutyutyu/zenget//renovate/zenget#<zenget-release>"
  ]
}
```

Replace `<zenget-release>` with an exact released tag. The unpinned form
(`github>tyutyutyu/zenget//renovate/zenget`) follows the default branch and is
for development or preset testing only. Repositories that restrict
`enabledManagers` must also enable `custom.regex`:

```json
{
  "extends": [
    "github>tyutyutyu/zenget//renovate/zenget#<zenget-release>"
  ],
  "enabledManagers": ["custom.regex"]
}
```

Renovate only proposes a pull request; it does not install anything, merge
automatically, or run `postUpgradeTasks`. Review the repository, tag, release
contents, and diff before merging, then decide separately whether to run
`zenget apply`. The preset does not change asset/platform/archive/target,
recipe, policy, checksum, or lockfile data. When using TASK-0050, regenerate
and review the lockfile in a separate workflow after accepting a manifest tag
update:

```sh
zenget lock zenget.json --output zenget-lock.json --force
zenget apply zenget.json --lock zenget-lock.json --dry-run
```

See [`renovate/README.md`](renovate/README.md) for the full workflow, security
boundary, and fixture-backed validation commands.

Inspect and clean the local extracted-artifact cache with `cache list` and
`cache gc`:

```sh
zenget cache list
zenget cache list --json
zenget cache gc --dry-run
zenget cache gc --max-age 720h --max-bytes 2GiB
zenget cache gc --manifest ./zenget.json --dry-run
```

The cache is separate from the active installation directory and is stored
under `${XDG_DATA_HOME:-~/.local/share}/zenget/cache`. A successful install,
restore, or manifest apply reuses an exact provider, repository, tag,
platform/libc, and asset match when the cached extracted binary passes its
recorded SHA-256 and XXH3-64 checks. A cache miss or integrity failure follows
the normal verified download path and republishes the result only after
extraction succeeds. Uninstall removes the active wrapper/state and managed
installed artifact, but intentionally leaves reusable cache content in place.

Installed wrappers are managed shims. Their runtime is copied atomically to
`${XDG_DATA_HOME:-~/.local/share}/zenget/shim/runtime`, so invoking a wrapper
does not depend on `zenget` being discoverable through `PATH`. A successful
install, upgrade, restore, or manifest apply refreshes the runtime and
generates a wrapper that preserves the target's arguments and environment.
Older wrappers are migrated by the next successful operation that updates
that target.

When a managed wrapper starts, zenget resolves the invoked target from the
nearest project manifest, using `ZENGET_MANIFEST` when set; a valid manifest
entry wins only when its target name is an exact match. If the manifest has no
entry for that target, the globally active installation is used. Set
`ZENGET_NO_MANIFEST_DISCOVERY=1` (or use `--no-manifest-discovery` with
`which`) to skip automatic manifest discovery. Ambiguous matches fail closed.
Project selections execute only an exact provider, repository, tag,
platform/libc, and target cache entry; a missing or damaged entry never
downloads or installs at launch. Run `zenget apply` to materialize it.

Inspect the same side-effect-free selection without executing the target:

```sh
zenget which repo
zenget which repo --json
zenget which repo --no-manifest-discovery
```

The report includes the source (project or global), manifest, provider,
repository, tag, artifact path, and integrity status. Usage events written by
the dispatcher include the repository and selected version in addition to the
start time, duration, and exit code.

`cache gc` is conservative by default: without `--max-age` or `--max-bytes` it
only removes damaged indexed artifacts and abandoned staging files older than
24 hours. An explicit age or byte budget may remove only unprotected entries;
active and retained previous state artifacts, plus exact entries referenced by
the selected manifest, are protected. `--dry-run` always prints the plan
without mutation. Manifest files are loaded strictly before an actual GC, so
an invalid explicit or discovered manifest stops the command before anything
is changed. Artifact acquisition is serialized per repository/release/target
platform; a waiting process reports a lock timeout rather than using partial
staging content.

Manifest schema version 1 has this shape (the `asset`, platform, archive
member, and target fields are optional when the normal install selection is
desired):

```json
{
  "$schema": "https://raw.githubusercontent.com/tyutyutyu/zenget/main/schemas/manifest-v1.schema.json",
  "schema_version": 1,
  "apps": [
    {
      "repository": "org/repo",
      "provider": "github",
      "tag": "v1.2.3",
      "asset": "repo-linux-amd64.tar.gz",
      "platform_os": "linux",
      "platform_arch": "amd64",
      "archive_binary": "bin/repo",
      "target_name": "repo"
    }
  ]
}
```

The manifest contains no token, usage data, download URL, local path,
installation timestamp, or binary hash. Unknown fields and future schema
versions are rejected. URLs, stdin, and arbitrary asset-download sources are
not accepted by `apply`; a failed entry produces a non-zero exit status while
other entries continue.

List everything zenget has installed:

```sh
zenget list [--json]
```

Add `--updates` (or `-u`) to check GitHub for a newer release of each installed app. This adds a `LATEST` column showing the latest upstream tag and its age, for example `v1.4.0 (5d old)`. The `VERSION` column is color-coded: green when up to date, yellow when only the patch differs, orange when the minor version differs, and red when the major version differs. A failed lookup shows `?` for that row and does not abort the command.

Coloring follows the `--color` flag and the `NO_COLOR` environment variable: by default (`--color auto`) zenget writes ANSI color codes only when standard output is a terminal and `NO_COLOR` is not set to a non-empty, non-`0` value (see https://no-color.org/). `--color always` colors even when output is piped and overrides `NO_COLOR`; `--color never` disables colors entirely. An invalid `--color` value is rejected before anything is printed. `--json` output never contains color codes.

```sh
zenget list --updates
zenget list --json --updates
zenget list --updates --color always   # force colors even when piped
```

For scripts and CI, add `--check` to `--updates`. The command prints the same
table or JSON output, then exits with status 0 when every installed app is
current, 3 when at least one app is behind its latest release, or 1 when any
latest-release lookup is indeterminate because of an API, network, or malformed
state error. `--check` requires `--updates`.

```sh
zenget list --updates --check
zenget list --json --updates --check
```

Add `--unused` to see apps that have not been used in the last 90 days. Apps are grouped by age: never used, 1 year or more (365+ days), 6-12 months (180-364 days), and 3-6 months (90-179 days). The output is sorted with never-used apps first, then by days since last use in descending order. `--unused` ignores `--updates` and can be combined with `--json`.

```sh
zenget list --unused
zenget list --unused --json
```

Remove an installed binary and its registry entry:

```sh
zenget uninstall org/repo
zenget uninstall repo
```

A short repository name is resolved case-sensitively when it uniquely matches
an installed registry entry. If multiple organizations have an installed
repository with that name, zenget lists every matching `org/repo` and removes
nothing; use the full repository name to choose one.

Check installed applications and restore missing or modified files using the
release tag recorded in state:

```sh
zenget ensure
zenget ensure org/repo
```

Switch an application back to the retained previous version without contacting
GitHub or downloading anything:

```sh
zenget rollback org/repo
```

Rollback requires both the active and previous versioned artifacts. zenget
checks their executable status and recorded hashes, verifies the managed
wrapper, and then swaps the two state entries atomically. Missing, modified, or
foreign files abort without changing the wrapper, state, or artifacts. Only
the active version and one previous version are retained, so two consecutive
rollbacks switch deterministically between those versions.

Diagnose the local configuration, registry, wrappers, and installed binaries
without contacting GitHub or changing files:

```sh
zenget doctor
zenget doctor org/repo
zenget doctor --json
```

With no repository argument, every registry entry is checked; arguments limit
the report to those repositories. A healthy or empty installation exits 0.
Missing or invalid entries exit 1 and are still included in the complete,
parseable JSON report. The JSON uses `schema_version: 1` and stable problem
codes such as `invalid_config`, `invalid_state`, `not_installed`,
`missing_binary`, `hash_mismatch`, `missing_wrapper`, `foreign_wrapper`, and
`io_error`. `doctor` is strictly read-only and performs no upstream or network
checks.

Remove registry entries whose wrapper and real binary are both missing:

```sh
zenget prune --dry-run
zenget prune --force
```

Prune never deletes files. Partially missing applications remain registered
with a warning and a suggestion to run `zenget ensure org/repo`. On a terminal,
`prune` asks for y/N confirmation; in non-interactive use, pass `--force` to
allow registry changes.

## Behavior

- **Version pinning.** By default zenget installs the latest GitHub release. Use `--tag` to install an exact release or the most recent tag containing the given string.
- **Release forges and API endpoints.** `install`, `inspect`, and `list --updates` accept `--api-base-url` for GitHub Enterprise (`/api/v3`), Forgejo/Gitea (`/api/v1`) or GitLab (`/api/v4`). Use `--forge github`, `--forge gitlab`, or `--forge forgejo` to select a dialect explicitly; when omitted, `gitlab.com` and `/api/v4`/`/api/v1` API paths are detected automatically. GitHub-compatible Forgejo/Gitea releases use the existing `repos/.../releases` schema. GitLab uses the project releases API and maps `assets.links[].direct_asset_url` (falling back to `url`) into zenget's common asset model. The repository argument remains `org/repo`; full project URLs are not accepted.
- **Minimum release age.** Use `--min-age-days N` to install only a stable release published at least N days ago. This helps avoid consuming a compromised or accidental release immediately after publication. It cannot be combined with `--tag`; the age-filtered lookup considers only the first page of releases returned by GitHub.
- **Smart asset selection.** Releases may contain multiple assets. zenget filters out checksums, signatures, installer scripts, source archives and unsupported package formats, then applies an optional reusable `--asset-match` substring or `--asset-match-regex` RE2 selector before selecting the asset whose name matches the current platform. On Darwin, `universal` and `all` assets match both supported architectures but an architecture-specific asset wins when present. On 64-bit platforms, explicitly 32-bit `i386`/`i686`/`x86` assets are excluded when a compatible alternative remains. On a Linux host using musl, explicitly `gnu`/`glibc`-marked assets are excluded when a compatible candidate remains, and `musl`-marked assets are preferred over libc-neutral assets. If only `gnu`/`glibc`-marked Linux assets remain, selection fails and lists those candidates. On glibc or an unrecognized/non-Linux host, libc tokens do not change the existing selection behavior. When several compatible assets match, `.tar.gz`/`.tgz` is preferred over `.zip` over raw binaries, with ties broken by name. Use `--asset` to select a specific asset by exact name or unique substring.
- **Cross-platform asset selection.** `--system os/arch` changes only the OS/architecture match; it has no libc dimension, so the target host's libc cannot be inferred or guaranteed. In particular, use `--asset` when the target's libc-specific asset must be selected explicitly.
- **Supported formats.** The asset may be a raw binary, a compressed raw binary (`.gz`, `.bz2`, `.xz`, `.zst`), a `.tar`/`.tar.gz`/`.tgz`/`.tar.bz2`/`.tar.xz`/`.tar.zst`/`.tzst` archive, or a `.zip` archive. For archives, zenget first selects an executable whose basename exactly matches the repository name, then a single executable whose basename starts with that name. If the archive contains exactly one executable, it is used as a fallback; otherwise, interactive installs show the executable candidates and non-interactive installs must use `--bin-name`. `--name` controls the stable wrapper name and defaults to the repository name.
- **Checksum-verified downloads.** Before installing the selected asset, zenget verifies every available SHA-256 checksum published by the release. This includes GitHub's asset metadata digest (`sha256:<64 hex>`), a per-asset sidecar file named `<asset>.sha256`, and aggregate files such as `sha256.sum`, `checksums.txt`, `SHA256SUMS`, or `checksums.sha256`. The default `if-present` policy allows a complete absence of checksum evidence; malformed, wrong-target, or mismatching evidence aborts the install. `required` also rejects assets without a valid, unambiguous SHA-256 source, and both modes fail before extraction or persistent changes.
- **Resilient downloads.** Release and checksum assets retry a bounded number of transient network, 408, 429, and 5xx failures. If a response is interrupted, zenget resumes from the bytes already written when the server returns a matching `206`/`Content-Range`; a server that ignores `Range` and returns `200` is handled by restarting from zero. Invalid ranges, repeated truncation, exhausted retries, and cancellation remove only the temporary partial file and never replace the destination.
- **Foreign-file protection.** If a file already exists at the install path and was not installed by zenget, zenget refuses to overwrite it.
- **Atomic install.** The versioned artifact and stable wrapper are each written to temporary files and renamed into place with mode `0755`.
- **Hash-verified uninstall.** zenget records the XXH3-64 hash of every installed binary. `uninstall` refuses to remove a binary whose hash no longer matches, so files modified after install are left untouched.
- **Versioned artifact storage.** User-facing wrappers keep a stable path, while binaries are stored below `$XDG_DATA_HOME/zenget/bin/<provider>/<owner>/<repo>/<version>/<target>`. Upgrades retain the active binary and exactly one verified previous version; legacy unversioned state and artifacts are migrated safely and remain readable.
- **Implicit upgrade.** Re-running `zenget install org/repo` upgrades to the latest release when a newer tag exists; if the installed version is already current, it reports that and does nothing.
- **Batch upgrade.** `zenget upgrade` (or `zenget upgrade org/repo ...`) plans updates for unpinned applications, asks for one confirmation in a terminal, and installs them sequentially in sorted repository order. `--dry-run` performs release and saved-choice validation without downloading or mutating files; non-interactive execution requires `--yes`. The summary reports updated, unchanged, skipped, and failed entries, and failures return a non-zero exit status.
- **Portable manifests.** `zenget init` creates an empty local manifest and `zenget add` records an exact release choice without installing it. `zenget manifest path` reports the selected project manifest; `add` and `apply` use explicit `--manifest`, `ZENGET_MANIFEST`, or nearest-`zenget.json` discovery, with `--no-manifest-discovery` as an opt-out. `zenget export` writes deterministic schema-version 1 installation intentions. `zenget apply FILE` also accepts a positional local path, resolves exact recorded tags, and applies recorded asset/platform/archive/target choices. `--dry-run` is read-only; manifest entries are sorted by repository, unlisted applications are preserved, and failed entries do not prevent the remaining entries from being attempted. The public Draft 2020-12 schema is available at `schemas/manifest-v1.schema.json`.
- **Trusted project execution.** Project target shims are created only after `zenget project trust` records a safe canonical manifest path and `zenget project activate` completes its full offline preflight. Lazy execution is disabled by default; with `lazy_install=true`, a trusted cache miss downloads only the exact manifest tag through GitHub, applies checksum and resource-limit policy, and atomically publishes the extracted artifact without modifying global state. `ZENGET_LAZY_INSTALL=0` disables it for one invocation. `project trust list` and `project untrust` manage the local trust records.
- **Commit-pinned recipe registries.** `zenget registry add/remove/list` manages named `github:owner/repository@<40-hex-commit>:<relative-root>` sources without network access. `registry sync` verifies the default-deny recipe policy, strict v1 `registry.json`, bounded Contents responses, recipe SHA-256 values, and repository/schema agreement before publishing an immutable snapshot. `install --registry NAME` selects exactly one snapshot fail-closed; `registry sync NAME --offline` validates it locally, and `install --offline --registry NAME` uses only the policy-authorized snapshot plus a verified artifact cache entry. A plain `install org/repo` automatically uses the first synchronized snapshot whose index lists the repository (printing its provenance) and silently falls back to heuristic resolution on a miss, an unsynced or corrupt snapshot, or a policy denial; `--no-registry` disables only that automatic lookup.
- **Artifact lockfiles.** `zenget lock MANIFEST --output FILE` writes a deterministic schema-version 1 lock containing exact provider asset metadata and archive SHA-256 values. `zenget apply MANIFEST --lock FILE` rejects schema, selection, tag, platform, or asset drift and validates all artifact content before installing; dry-run reports content verification as pending.
- **Artifact cache.** `zenget cache list [--json]` reports indexed extracted artifacts and their integrity/state/manifest protection. `zenget cache gc` performs an explicit, deterministic dry-run or cleanup; normal installs, restores, and applies reuse only hash-verified exact entries. The cache is separate from active binaries, survives uninstall, protects active/rollback/manifest references, and removes age/size candidates only when the corresponding GC budget is explicitly requested.
- **Pinning.** `zenget pin org/repo` prevents implicit upgrades for an installed app. `--tag` is an explicit override and keeps the pin; `zenget unpin org/repo` re-enables normal upgrades. Pinned apps are marked `(pinned)` in `list` and expose `"pinned": true` in JSON output.
- **Ensure.** `zenget ensure` checks the real binary hash and managed wrapper for every registered app, then restores unhealthy apps from their recorded release tag. It never upgrades versions; a summary is printed and any failed restoration returns a non-zero exit status.
- **Offline rollback.** `zenget rollback org/repo` swaps the active and retained previous artifacts without a network request or download. It validates both hashes and the managed wrapper before changing anything, preserves pinning, app selection, and usage data, and supports only the two retained versions.
- **Doctor.** `zenget doctor` performs a local, read-only consistency check of configuration, state, wrappers, and installed binaries. It reports stable problem codes in human or `schema_version: 1` JSON output, never contacts GitHub, and exits non-zero when any requested or registered application is unhealthy.
- **Prune.** `zenget prune` removes only registry entries whose wrapper and real binary are both absent. It keeps healthy and partially missing entries, warns about the latter with an `ensure` suggestion, never deletes files, and requires terminal confirmation or `--force`; `--dry-run` is read-only.

## Configuration

- **State file:** `$XDG_CONFIG_HOME/zenget/state.json` (defaults to `~/.config/zenget/state.json` when `XDG_CONFIG_HOME` is unset). It records every installed app's version, install time, download URL, stable wrapper path, active and previous versioned artifact paths, XXH3-64 hashes, and the selected target platform when one is known. Older state files remain valid and are upgraded on the next install.
- **Per-application choices:** `${XDG_CONFIG_HOME:-~/.config}/zenget/apps/<org>__<repo>.json` stores the legacy concrete `asset` choice or a reusable typed `asset_selector`, together with the archive member and target name. `inspect` reads this file without modifying it; explicit asset or selector flags take precedence.
- **Install directory:** configurable. Use `zenget config set install_dir <path>` to change where wrapper binaries are placed. The value must be an absolute path or start with `~/` (e.g. `zenget config set install_dir ~/bin`). When unset, wrappers are installed to `~/.local/bin` as before. Changing `install_dir` and then upgrading an app removes the wrapper from the previous location (only if it is a zenget-managed wrapper; foreign files are never deleted).
- **Checksum policy:** `zenget config set checksum_policy if-present` keeps the backward-compatible default, while `required` demands valid SHA-256 evidence for every normal release asset. The install flag overrides the global value for that install; a missing field means `if-present`, and `off` is not accepted. Recipe checksums and lockfile archive hashes remain mandatory independently.
- **Project lazy installation:** `lazy_install` defaults to `false`; set it with `zenget config set lazy_install true` to allow trusted project shims to materialize exact manifest artifacts on first execution. `ZENGET_LAZY_INSTALL=0` is a per-process opt-out. Lazy execution still requires a safe trust record, uses the global checksum and resource-limit settings, and does not update the global registry.
- **Recipe registries:** `${XDG_CONFIG_HOME:-~/.config}/zenget/registries.json` stores only sorted named commit-pinned sources. Immutable registry snapshots live below `${XDG_CACHE_HOME:-~/.cache}/zenget/registries`; sync publishes a new snapshot only after all index and recipe checks pass. `registry sync NAME --offline` rechecks the current policy, regular files, hashes, and recipe schema without contacting GitHub.
- **Usage tracking:** opt-in. Set `usage_tracking=true` with `zenget config set usage_tracking true`. When enabled, each installed wrapper appends one JSONL event (`start`, `duration_s`, `exit_code`, `repository`, `version`) to `$XDG_DATA_HOME/zenget/usage/<provider>/<owner>/<repo>.jsonl` on every invocation. Stats, unused-app checks, and uploads also read the legacy basename log so existing history is preserved. This stays local by default.
- **Usage upload:** opt-in. Set `usage_upload=true` with `zenget config set usage_upload true`. This requires `usage_tracking` to also be enabled. When both are on, raw events are sent at most once per 24 hours to a central collector. The endpoint is a placeholder (`https://telemetry.zenget.example/v1/usage`) until a production collector exists; override it with `ZENGET_USAGE_URL` for testing or custom collectors. Each upload gets a fresh random send ID; a single persistent random user ID is generated once and stored under `$XDG_DATA_HOME/zenget/upload/user_id`. The watermark file `$XDG_DATA_HOME/zenget/upload/watermark.json` records the last successful send so only new events are transmitted. Failures are silent to the CLI and retried at the next opportunity.
- **GitHub authentication:** optional. Set `ZENGET_GITHUB_TOKEN` (or `GITHUB_TOKEN`, checked in that order) to authenticate API and private release-asset requests as `Authorization: Bearer <token>`. For a private repository, use a token with access to that repository and the minimum `Contents: read` permission (for a fine-grained personal access token). zenget keeps the token in memory for the request only; it is not written to output, errors, state, or configuration. Without a token, requests are anonymous and subject to GitHub's 60 requests/hour rate limit.
- **Other forge authentication:** optional for public releases. GitLab checks `CI_JOB_TOKEN` first (sent as `JOB-TOKEN`), then `GITLAB_TOKEN` (sent as `PRIVATE-TOKEN`). Forgejo/Gitea checks `FORGEJO_TOKEN` first, then `CODEBERG_TOKEN` (sent as token authorization). Tokens are kept in memory for the request only. GitLab release assets use the returned direct or browser URL; private asset permissions and provider-specific download details remain subject to the forge's URL and token policy.
- **Resource limits:** every structured JSON input is limited to 4 MiB, each checksum asset to 8 MiB, each release download to 1 GiB, each extracted binary candidate to 512 MiB, each archive to 10,000 entries and 2 GiB of decompressed data. The keys are `max_structured_bytes`, `max_checksum_bytes`, `max_download_bytes`, `max_binary_bytes`, `max_archive_entries`, and `max_archive_bytes`; `zenget config list` shows their effective values and `zenget config get <key>` reads one value. Set a finite positive value with `zenget config set max_download_bytes 2GiB` (or a decimal byte/count value). Values are persisted as decimal JSON integers; zero, negative, overflowing, `unlimited`, and unknown units are rejected. Raise only the relevant limit for a known-large release, for example `zenget config set max_download_bytes 2GiB`.
- **Artifact cache:** immutable extracted binaries and the atomically replaced cache index live under `$XDG_DATA_HOME/zenget/cache` (or `~/.local/share/zenget/cache`). Staging files are bounded by GC's documented 24-hour orphan threshold; an active acquisition lock waits up to 30 seconds before reporting a timeout. Cache GC never follows or removes a symlink in place of a managed artifact.

### Upload payload contract

A successful upload `POST`s JSON with this shape:

```json
{
  "schema_version": 1,
  "send_id": "<fresh-128-bit-hex>",
  "user_id": "<persistent-128-bit-hex>",
  "sent_at": "2026-09-01T12:00:00Z",
  "zenget_version": "0.0.1",
  "apps": [
    {
      "app": "org/repo",
      "events": [
        {"start": "2026-09-01T11:59:00Z", "duration_s": 5, "exit_code": 0}
      ]
    }
  ]
}
```

The collector deduplicates by `send_id`. Success is HTTP 2xx; any other status, timeout, or network error is treated as a failure and retried later.

<!-- Temporary fork CI/CodeQL permission probe; do not merge. -->
