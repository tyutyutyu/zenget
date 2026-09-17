#!/usr/bin/env bash
# Download and exercise one public Unix release, optionally upgrading from an older one.
set -euo pipefail

tag="${1:-}"
previous_tag="${2:-}"
archive_os="${3:-}"
archive_arch="${4:-}"
repository="${GITHUB_REPOSITORY:-tyutyutyu/zenget}"

if [[ -z "$tag" || -z "$archive_os" || -z "$archive_arch" ]]; then
  echo "usage: $0 TAG [PREVIOUS_TAG] ARCHIVE_OS ARCHIVE_ARCH" >&2
  exit 2
fi

if [[ "$archive_os" == "windows" ]]; then
  echo "self-upgrade smoke is intentionally not supported on Windows" >&2
  exit 2
fi

sha256_file() {
  local file_path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file_path" | awk '{print tolower($1)}'
  else
    shasum -a 256 "$file_path" | awk '{print tolower($1)}'
  fi
}

download_release() {
  local release_tag="$1"
  local output_dir="$2"
  local release_version="${release_tag#v}"
  local archive="zenget_${release_version}_${archive_os}_${archive_arch}.tar.gz"
  local base="https://github.com/${repository}/releases/download/${release_tag}"
  mkdir -p "$output_dir"
  if ! curl --fail --location --retry 3 --proto '=https' --proto-redir '=https' "$base/checksums.txt" --output "$output_dir/checksums.txt"; then
    echo "failed to download public release checksums for $release_tag" >&2
    return 1
  fi
  if ! curl --fail --location --retry 3 --proto '=https' --proto-redir '=https' "$base/$archive" --output "$output_dir/$archive"; then
    echo "failed to download public release archive $archive" >&2
    return 1
  fi
  local expected actual
  expected="$(awk -v asset="$archive" '$2 == asset { print tolower($1); exit }' "$output_dir/checksums.txt")"
  actual="$(sha256_file "$output_dir/$archive")"
  if [[ -z "$expected" || "$expected" != "$actual" ]]; then
    echo "SHA-256 mismatch for public release asset $archive" >&2
    return 1
  fi
  tar -xzf "$output_dir/$archive" -C "$output_dir"
  chmod +x "$output_dir/zenget"
  printf '%s\n' "$output_dir/zenget"
}

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

current_binary="$(download_release "$tag" "$tmpdir/current")"
"$current_binary" --help >/dev/null

if [[ -z "$previous_tag" ]]; then
  echo "::notice::No PREVIOUS_TAG supplied; checksum/help smoke passed, self-upgrade smoke was not run."
  exit 0
fi

old_binary="$(download_release "$previous_tag" "$tmpdir/previous")"
mkdir -p "$tmpdir/config"
self_upgrade_output="$(XDG_CONFIG_HOME="$tmpdir/config" "$old_binary" self-upgrade)"
if ! grep -Fq " to $tag " <<<"$self_upgrade_output"; then
  printf 'self-upgrade output did not confirm target %s:\n%s\n' "$tag" "$self_upgrade_output" >&2
  exit 1
fi

echo "$self_upgrade_output"
echo "Public checksum, $archive_os/$archive_arch help, and self-upgrade smoke passed."
