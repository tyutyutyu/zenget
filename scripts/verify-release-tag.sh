#!/usr/bin/env bash
# Validate the immutable source/tag relationship used by the release workflow.
set -euo pipefail

tag="${1:-${GITHUB_REF_NAME:-}}"
commit="${2:-${GITHUB_SHA:-}}"

if [[ -z "$tag" || -z "$commit" ]]; then
  echo "usage: $0 TAG COMMIT" >&2
  exit 2
fi

if [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "release tag must be an exact vX.Y.Z semantic version: $tag" >&2
  exit 1
fi

if ! git rev-parse --verify --quiet "${tag}^{commit}" >/dev/null; then
  echo "release tag is not present locally: $tag" >&2
  exit 1
fi

if [[ "$(git cat-file -t "$tag")" != "tag" ]]; then
  echo "release tag must be annotated: $tag" >&2
  exit 1
fi

tag_object="$(git cat-file -p "$tag")"
if ! grep -Eq '^(gpgsig|sshsig|x509sig) ' <<<"$tag_object"; then
  echo "release tag must carry a GPG, SSH, or X.509 signature: $tag" >&2
  exit 1
fi

tag_commit="$(git rev-parse --verify "${tag}^{commit}")"
if [[ "$tag_commit" != "$commit" ]]; then
  echo "tag $tag resolves to $tag_commit, but the event points to $commit" >&2
  exit 1
fi

git fetch --no-tags origin main:refs/remotes/origin/main
if ! git merge-base --is-ancestor "$tag_commit" refs/remotes/origin/main; then
  echo "tag $tag does not point to a commit reachable from origin/main" >&2
  exit 1
fi

other_tags=()
while IFS= read -r candidate; do
  [[ "$candidate" == "$tag" ]] && continue
  if [[ "$(git rev-parse --verify "${candidate}^{commit}")" == "$tag_commit" ]]; then
    other_tags+=("$candidate")
  fi
done < <(git for-each-ref --format='%(refname:strip=2)' refs/tags)
if [[ "${#other_tags[@]}" -gt 0 ]]; then
  printf 'release commit %s already has another tag:\n' "$tag_commit" >&2
  printf '  %s\n' "${other_tags[@]}" >&2
  exit 1
fi

source_version="$(awk -F'"' '/^[[:space:]]*const Version = / { print $2; exit }' internal/version/version.go)"
if [[ -z "$source_version" || "$tag" != "v$source_version" ]]; then
  echo "tag $tag does not match internal/version/version.go Version $source_version" >&2
  exit 1
fi

printf 'Validated signed annotated tag %s at %s (source Version %s) on origin/main.\n' \
  "$tag" "$tag_commit" "$source_version"
