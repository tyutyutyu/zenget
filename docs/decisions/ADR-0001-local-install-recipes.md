# ADR-0001: Local versioned install recipes

- Status: Accepted
- Date: 2026-09-07

## Context

GitHub releases do not share one asset, archive-member, target-name, or
checksum naming convention. Per-application configuration stores a user's
resolved choices, but it is not a portable contract for a repository's
release convention. A recipe must remain safe to review and use without
introducing a remote registry or arbitrary code execution.

## Decision

zenget supports an explicit local JSON recipe with `schema_version: 1` and an
exact `repository: "org/repo"`. The optional fields are:

- `supported_systems`, a normalized allowlist of supported `os/arch` values;
- `asset_selector`, reusing the existing versioned substring/RE2 selector;
- `archive_binary`, a safe archive filename or relative archive path;
- `target_name`, a safe single-component installed command name; and
- `checksum`, an exact or RE2 selector for one current-release asset plus a
  `raw` or `sha256sum` SHA-256 format.

Parsing is strict: unknown schema versions/fields, duplicate JSON keys,
invalid values, unsafe paths, symlinks, directories, URLs, and stdin are
rejected before provider access or installation mutation. The precedence is
explicit CLI choice, saved application choice, recipe rule, then automatic
heuristics. The recipe checksum asset must resolve to exactly one current
release asset and is downloaded via the existing authenticated provider path.

Recipe contents and paths are not persisted. A recipe without a checksum keeps
the existing automatic checksum verification behavior. A recipe contains
selection rules, not an exact release lock or artifact-provenance evidence.

## Alternatives considered

- Extending app configuration would make a user-specific state file the
  portable recipe format and would blur resolved choices with reusable rules.
- A remote registry would add discovery, trust, availability, and credential
  concerns outside this feature's scope.
- A manifest/lockfile would solve exact release reproducibility, but would
  duplicate the existing workflow rather than describe reusable selection
  conventions.

## Consequences

Recipes can be reviewed, versioned, and reused locally while remaining bounded
to GitHub release metadata and existing download/verification behavior. Schema
changes require a new schema version. Exact reproducibility and provenance
remain the responsibility of the manifest lock workflow.
