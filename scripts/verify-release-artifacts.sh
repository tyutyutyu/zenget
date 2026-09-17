#!/usr/bin/env bash
# Verify GoReleaser's archive matrix and checksum manifest.
set -euo pipefail

usage() {
  echo "usage: $0 DIST [--run-help]"
}

dist="${1:-}"
run_help=0
shift || true
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --run-help) run_help=1 ;;
    *) usage >&2; exit 2 ;;
  esac
  shift
done

if [[ -z "$dist" || ! -d "$dist" ]]; then
  usage >&2
  exit 2
fi

checksum_file="$dist/checksums.txt"
if [[ ! -s "$checksum_file" ]]; then
  echo "missing non-empty checksum file: $checksum_file" >&2
  exit 1
fi

shopt -s nullglob
archives=("$dist"/*.tar.gz "$dist"/*.zip)
if [[ "${#archives[@]}" -ne 5 ]]; then
  printf 'expected exactly five release archives, found %s\n' "${#archives[@]}" >&2
  exit 1
fi

declare -A expected_platforms=(
  [linux_amd64.tar.gz]=0
  [linux_arm64.tar.gz]=0
  [darwin_amd64.tar.gz]=0
  [darwin_arm64.tar.gz]=0
  [windows_amd64.zip]=0
)

for archive in "${archives[@]}"; do
  name="$(basename "$archive")"
  platform="${name#zenget_}"
  platform="${platform#*_}"
  if [[ -z "${expected_platforms[$platform]+set}" ]]; then
    echo "unexpected archive name or platform: $name" >&2
    exit 1
  fi
  expected_platforms[$platform]=$((expected_platforms[$platform] + 1))
done

for platform in "${!expected_platforms[@]}"; do
  if [[ "${expected_platforms[$platform]}" -ne 1 ]]; then
    echo "expected one archive for $platform" >&2
    exit 1
  fi
done

mapfile -t archive_names < <(printf '%s\n' "${archives[@]##*/}" | sort)
mapfile -t checksum_names < <(
  awk 'NF == 2 { gsub(/^\*/, "", $2); print $2 }' "$checksum_file" | sort
)
if [[ "${#checksum_names[@]}" -ne 5 ]]; then
  echo "checksums.txt must contain exactly five archive entries" >&2
  exit 1
fi
if ! diff -u <(printf '%s\n' "${archive_names[@]}") <(printf '%s\n' "${checksum_names[@]}"); then
  echo "checksums.txt does not cover exactly the produced archives" >&2
  exit 1
fi

if ! (cd "$dist" && sha256sum --check checksums.txt); then
  echo "release checksum verification failed" >&2
  exit 1
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

for archive in "${archives[@]}"; do
  name="$(basename "$archive")"
  archive_stem="${name%.tar.gz}"
  archive_stem="${archive_stem%.zip}"
  extract_dir="$tmpdir/$archive_stem"
  mkdir -p "$extract_dir"
  case "$name" in
    *.tar.gz)
      tar -xzf "$archive" -C "$extract_dir"
      mapfile -t entries < <(tar -tzf "$archive")
      executable_count=0
      for entry in "${entries[@]}"; do
        case "$entry" in
          zenget) executable_count=$((executable_count + 1)) ;;
          README.md) ;;
          *) echo "$name contains unexpected archive entry: $entry" >&2; exit 1 ;;
        esac
      done
      if [[ "$executable_count" -ne 1 ]]; then
        echo "$name must contain one root-level zenget executable" >&2
        exit 1
      fi
      if [[ ! -x "$extract_dir/zenget" ]]; then
        echo "$name executable is not executable after extraction" >&2
        exit 1
      fi
      ;;
    *.zip)
      unzip -q "$archive" -d "$extract_dir"
      mapfile -t entries < <(unzip -Z1 "$archive")
      executable_count=0
      for entry in "${entries[@]}"; do
        case "$entry" in
          zenget.exe) executable_count=$((executable_count + 1)) ;;
          README.md) ;;
          *) echo "$name contains unexpected archive entry: $entry" >&2; exit 1 ;;
        esac
      done
      if [[ "$executable_count" -ne 1 ]]; then
        echo "$name must contain one root-level zenget.exe" >&2
        exit 1
      fi
      ;;
    *)
      echo "unsupported archive type: $name" >&2
      exit 1
      ;;
  esac
done

if [[ "$run_help" -eq 1 ]]; then
  linux_name="$(printf '%s\n' "${archive_names[@]}" | awk '/_linux_amd64\.tar\.gz$/ { print; exit }')"
  linux_dir="$tmpdir/${linux_name%.tar.gz}"
  if [[ -x "$linux_dir/zenget" ]]; then
    "$linux_dir/zenget" --help >/dev/null
  else
    echo "Linux amd64 archive was not extracted to the expected path" >&2
    exit 1
  fi
fi

echo "Verified five platform archives, checksums.txt, and archive contents."
