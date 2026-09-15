# Renovate preset for zenget manifests

`renovate/zenget.json` is a shareable Renovate preset for repositories that
keep exact GitHub release choices in a canonical `zenget.json` manifest. The
preset uses one `customType: "regex"` manager. It captures the
`repository`/`tag` pair from each manifest object, looks up releases with the
`github-releases` datasource, and replaces only the captured `tag` value.

The manager intentionally relies on the field order emitted by the TASK-0054
canonical manifest serializer: `repository`, `provider`, then `tag`. This
keeps each dependency pair local to one object and avoids matching a
repository from one object with a tag from another. The file pattern accepts
root or nested files named exactly `zenget.json`; the normal zenget manifest
parser remains the final schema and safety check.

## Use a released preset

Pin the preset to a zenget release so later changes to the preset do not
silently change a consuming repository's update behavior:

```json
{
  "extends": [
    "github>tyutyutyu/zenget//renovate/zenget#<zenget-release>"
  ]
}
```

Replace `<zenget-release>` with the exact zenget release tag, for example
`v0.0.1`. The unpinned form below follows the provider's default branch and is
useful only for development or trying the next preset revision:

```json
{
  "extends": ["github>tyutyutyu/zenget//renovate/zenget"]
}
```

If the consuming repository restricts `enabledManagers`, it must explicitly
enable Renovate's custom regex manager:

```json
{
  "extends": [
    "github>tyutyutyu/zenget//renovate/zenget#<zenget-release>"
  ],
  "enabledManagers": ["custom.regex"]
}
```

The preset itself does not enable or host Renovate, configure credentials,
merge pull requests, or select a schedule. Renovate's `github-releases`
datasource queries the release list for each captured `owner/repo`; it has no
implicit versioning scheme here, so the preset explicitly selects `loose`
versioning. This accepts common `v`-prefixed SemVer tags and makes a best-effort
comparison for release tags such as `release-2025.09`. The replacement is the
new release tag string returned by the datasource, so the `v` prefix or a
documented tag prefix is not stripped by the preset.

## Workflow and boundaries

1. Add or update a canonical manifest with `zenget init`/`zenget add`, then
   commit the exact tag and the matching manifest choices.
2. Renovate opens a pull request when a newer GitHub release tag is available.
   Review the repository, tag, release contents, and generated diff before
   merging.
3. After the PR is merged, choose separately whether to run
   `zenget apply zenget.json` (or the repository's documented CI command).
   Renovate does not install or upgrade a binary.
4. If the repository uses the TASK-0050 lock workflow, regenerate and review
   the lockfile separately after accepting the manifest tag update:

   ```sh
   zenget lock zenget.json --output zenget-lock.json --force
   zenget apply zenget.json --lock zenget-lock.json --dry-run
   ```

   The preset never edits lockfiles and does not use `postUpgradeTasks` to
   generate them. A manifest tag PR and a lockfile refresh may therefore be
   reviewed as separate changes.

The regex does not change `asset`, `platform_os`, `platform_arch`,
`archive_binary`, or `target_name`. It does not match recipe or policy files,
does not configure `automerge`, and does not verify checksums or artifact
provenance. A Renovate PR is a proposed release-tag change; zenget's normal
checksum, lockfile, recipe, and policy checks remain the relevant installation
boundaries.

## Validation

From the zenget repository root, validate the strict JSON and the fixture-
backed RE2 behavior with:

```sh
jq -e . renovate/zenget.json
npx --yes --package renovate renovate-config-validator renovate/zenget.json
go test ./internal/manifest -run Renovate -count=1
```

The Go tests compile the preset's manager and file patterns with Go's RE2
implementation, check named captures and per-object isolation, exercise both
`v`-prefixed SemVer and non-standard release tags, and load the replaced output
through the TASK-0054 strict manifest parser. The negative fixture covers a
legacy schema, similar key names, and a repository/provider/tag split across
different objects; it must be rejected rather than updated as a valid
manifest.

The preset follows the official Renovate documentation for [regex custom
managers](https://docs.renovatebot.com/modules/manager/regex/), the
[`github-releases` datasource](https://docs.renovatebot.com/modules/datasource/github-releases/),
[shareable presets](https://docs.renovatebot.com/config-presets/), and
[`loose` versioning](https://docs.renovatebot.com/modules/versioning/loose/);
these references were checked on 2026-09-07.
