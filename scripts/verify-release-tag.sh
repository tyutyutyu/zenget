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

trusted_signers="${ZENGET_TRUSTED_GPG_FINGERPRINTS:-}"
if [[ -z "$trusted_signers" ]]; then
  echo "ZENGET_TRUSTED_GPG_FINGERPRINTS must name at least one trusted maintainer fingerprint" >&2
  exit 1
fi

verification_output=""
if ! verification_output="$(git verify-tag --raw "$tag" 2>&1)"; then
  printf 'cryptographic tag signature verification failed for %s:\n%s\n' "$tag" "$verification_output" >&2
  exit 1
fi

signer_fingerprint="$(awk '$1 == "[GNUPG:]" && $2 == "VALIDSIG" { print toupper($3); exit }' <<<"$verification_output")"
if [[ -z "$signer_fingerprint" ]]; then
  echo "tag $tag is not a verifiable GPG-signed tag; SSH/X.509 signatures are not enabled by this policy" >&2
  exit 1
fi

trusted_match=0
for trusted in ${trusted_signers//,/ }; do
  trusted="${trusted//[[:space:]]/}"
  if [[ "$trusted" =~ ^[[:xdigit:]]+$ && ( "${#trusted}" -eq 40 || "${#trusted}" -eq 64 ) && "${signer_fingerprint}" == "${trusted^^}" ]]; then
    trusted_match=1
    break
  fi
done
if [[ "$trusted_match" -ne 1 ]]; then
  echo "tag $tag was signed by untrusted GPG fingerprint $signer_fingerprint" >&2
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
