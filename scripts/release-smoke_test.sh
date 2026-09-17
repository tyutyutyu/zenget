#!/usr/bin/env bash
# Exercise release smoke scripts without downloading or executing public assets.
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
fixtures="$work/fixtures"
stubs="$work/stubs"
curl_log="$work/curl.log"
curl_write_log="$work/curl-write.log"
extract_log="$work/extract.log"
exec_log="$work/zenget.log"
mkdir -p "$fixtures" "$stubs"
trap 'rm -rf "$work"' EXIT

fail() {
  echo "release smoke test failed: $*" >&2
  exit 1
}

assert_contains() {
  local needle="$1"
  local haystack="$2"
  [[ "$haystack" == *"$needle"* ]] || fail "expected $needle in $haystack"
}

assert_fails() {
  if "$@"; then
    fail "command unexpectedly succeeded: $*"
  fi
}

make_release_fixture() {
  local tag="$1"
  local version="${tag#v}"
  local release_dir="$fixtures/$tag"
  local source_dir="$work/source-$version"
  local archive="zenget_${version}_linux_amd64.tar.gz"
  mkdir -p "$release_dir" "$source_dir"
  cat >"$source_dir/zenget" <<EOF
#!/usr/bin/env bash
exec zenget --fixture-version "$tag" "\$@"
EOF
  chmod +x "$source_dir/zenget"
  tar -czf "$release_dir/$archive" -C "$source_dir" zenget
  (cd "$release_dir" && sha256sum "$archive" >checksums.txt)
}

cat >"$stubs/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
output=""
url=""
proto=""
proto_redir=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    --proto) proto="$2"; shift 2 ;;
    --proto-redir) proto_redir="$2"; shift 2 ;;
    http://*|https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
[[ "$proto" == "=https" ]] || { echo "curl missing HTTPS transfer restriction" >&2; exit 64; }
[[ "$proto_redir" == "=https" ]] || { echo "curl missing HTTPS redirect restriction" >&2; exit 64; }
[[ "$url" == https://* ]] || { echo "curl used a non-HTTPS URL" >&2; exit 64; }
printf '%s\n' "$url" >>"$CURL_LOG"
asset="${url##*/}"
tag="${url%/*}"
tag="${tag##*/}"
cp "$FIXTURE_DIR/$tag/$asset" "$output"
printf '%s\n' "$asset" >>"$CURL_WRITE_LOG"
if [[ "${CURL_FAIL_ASSET:-}" == "$asset" ]]; then
  exit 22
fi
EOF
chmod +x "$stubs/curl"

cat >"$stubs/tar" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >>"\$EXTRACT_LOG"
exec "$(command -v tar)" "\$@"
EOF
chmod +x "$stubs/tar"

cat >"$stubs/zenget" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
version="$2"
shift 2
printf '%s %s\n' "$version" "$*" >>"$EXEC_LOG"
case "${1:-}" in
  --help) exit 0 ;;
  self-upgrade) printf 'upgraded from %s to %s completed\n' "$version" "$EXPECTED_TAG" ;;
  *) exit 64 ;;
esac
EOF
chmod +x "$stubs/zenget"

make_release_fixture v1.0.0
make_release_fixture v0.9.0

run_platform_smoke() {
  local tmp_parent="$work/tmp dir"
  mkdir -p "$tmp_parent"
  PATH="$stubs:$PATH" FIXTURE_DIR="$fixtures" CURL_LOG="$curl_log" CURL_WRITE_LOG="$curl_write_log" EXTRACT_LOG="$extract_log" EXEC_LOG="$exec_log" EXPECTED_TAG=v1.0.0 TMPDIR="$tmp_parent" \
    bash "$repository_root/scripts/release-platform-smoke.sh" "$@"
}

: >"$curl_log"
: >"$curl_write_log"
: >"$extract_log"
: >"$exec_log"
current_output="$(run_platform_smoke v1.0.0 '' linux amd64)"
assert_contains "No PREVIOUS_TAG supplied" "$current_output"
[[ "$(wc -l <"$curl_log")" -eq 2 ]] || fail "current smoke did not make two HTTPS downloads"
[[ "$(wc -l <"$exec_log")" -eq 1 ]] || fail "current smoke ran more than --help"
assert_contains "--help" "$(cat "$exec_log")"

: >"$curl_log"
: >"$curl_write_log"
: >"$extract_log"
: >"$exec_log"
previous_output="$(run_platform_smoke v1.0.0 v0.9.0 linux amd64)"
assert_contains "self-upgrade smoke passed" "$previous_output"
[[ "$(wc -l <"$curl_log")" -eq 4 ]] || fail "previous-tag smoke did not make four HTTPS downloads"
assert_contains "self-upgrade" "$(cat "$exec_log")"

for failed_asset in checksums.txt zenget_1.0.0_linux_amd64.tar.gz; do
  : >"$curl_log"
  : >"$curl_write_log"
  : >"$extract_log"
  : >"$exec_log"
  assert_fails env CURL_FAIL_ASSET="$failed_asset" PATH="$stubs:$PATH" FIXTURE_DIR="$fixtures" CURL_LOG="$curl_log" CURL_WRITE_LOG="$curl_write_log" EXTRACT_LOG="$extract_log" EXEC_LOG="$exec_log" EXPECTED_TAG=v1.0.0 \
    bash "$repository_root/scripts/release-platform-smoke.sh" v1.0.0 '' linux amd64
  assert_contains "$failed_asset" "$(cat "$curl_write_log")"
  [[ ! -s "$extract_log" ]] || fail "download failure for $failed_asset attempted extraction"
  [[ ! -s "$exec_log" ]] || fail "download failure for $failed_asset executed a fixture binary"
  if [[ "$failed_asset" == "checksums.txt" ]]; then
    [[ "$(wc -l <"$curl_log")" -eq 1 ]] || fail "checksum download failure requested an archive"
  fi
done

checksum_fixture="$fixtures/v1.0.0/checksums.txt"
cp "$checksum_fixture" "$checksum_fixture.saved"
printf '%064d  zenget_1.0.0_linux_amd64.tar.gz\n' 0 >"$checksum_fixture"
: >"$exec_log"
: >"$extract_log"
assert_fails run_platform_smoke v1.0.0 '' linux amd64
[[ ! -s "$extract_log" ]] || fail "checksum mismatch attempted extraction"
[[ ! -s "$exec_log" ]] || fail "checksum mismatch executed a fixture binary"
mv "$checksum_fixture.saved" "$checksum_fixture"

fallback="$work/shasum-only"
mkdir -p "$fallback"
for command in awk bash chmod cp dirname env grep gzip mkdir mktemp rm tar; do
  ln -s "$(command -v "$command")" "$fallback/$command"
done
cat >"$fallback/shasum" <<EOF
#!/usr/bin/env bash
shift 2
exec "$(command -v sha256sum)" "\$@"
EOF
chmod +x "$fallback/shasum"
: >"$exec_log"
PATH="$stubs:$fallback" FIXTURE_DIR="$fixtures" CURL_LOG="$curl_log" CURL_WRITE_LOG="$curl_write_log" EXTRACT_LOG="$extract_log" EXEC_LOG="$exec_log" EXPECTED_TAG=v1.0.0 TMPDIR="$work/tmp dir" \
  bash "$repository_root/scripts/release-platform-smoke.sh" v1.0.0 '' linux amd64 >/dev/null

verifier_dist="$work/verifier-dist"
make_verifier_dist() {
  local dist="$1"
  local source="$work/verifier-source"
  rm -rf "$dist" "$source"
  mkdir -p "$dist" "$source"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$source/zenget"
  chmod +x "$source/zenget"
  printf 'release fixture\n' >"$source/README.md"
  for platform in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
    tar -czf "$dist/zenget_1.0.0_${platform}.tar.gz" -C "$source" zenget README.md
  done
  printf 'release fixture\n' >"$source/zenget.exe"
  (cd "$source" && zip -q "$dist/zenget_1.0.0_windows_amd64.zip" zenget.exe README.md)
  (cd "$dist" && sha256sum *.tar.gz *.zip >checksums.txt)
}

write_checksums() {
  (cd "$1" && sha256sum *.tar.gz *.zip >checksums.txt)
}

make_verifier_dist "$verifier_dist"
bash "$repository_root/scripts/verify-release-artifacts.sh" "$verifier_dist" --run-help >/dev/null

bad_hash="$work/bad-hash"
cp -R "$verifier_dist" "$bad_hash"
awk 'NR == 1 { printf "%064d  %s\n", 0, $2; next } { print }' "$bad_hash/checksums.txt" >"$bad_hash/checksums.txt.tmp"
mv "$bad_hash/checksums.txt.tmp" "$bad_hash/checksums.txt"
assert_fails bash "$repository_root/scripts/verify-release-artifacts.sh" "$bad_hash"

extra_archive="$work/extra-archive"
cp -R "$verifier_dist" "$extra_archive"
cp "$extra_archive/zenget_1.0.0_linux_amd64.tar.gz" "$extra_archive/zenget_1.0.0_freebsd_amd64.tar.gz"
write_checksums "$extra_archive"
assert_fails bash "$repository_root/scripts/verify-release-artifacts.sh" "$extra_archive"

unknown_platform="$work/unknown-platform"
cp -R "$verifier_dist" "$unknown_platform"
mv "$unknown_platform/zenget_1.0.0_linux_arm64.tar.gz" "$unknown_platform/zenget_1.0.0_freebsd_arm64.tar.gz"
write_checksums "$unknown_platform"
assert_fails bash "$repository_root/scripts/verify-release-artifacts.sh" "$unknown_platform"

missing_executable="$work/missing-executable"
cp -R "$verifier_dist" "$missing_executable"
missing_source="$work/missing-source"
mkdir -p "$missing_source"
printf 'release fixture\n' >"$missing_source/README.md"
tar -czf "$missing_executable/zenget_1.0.0_linux_amd64.tar.gz" -C "$missing_source" README.md
write_checksums "$missing_executable"
assert_fails bash "$repository_root/scripts/verify-release-artifacts.sh" "$missing_executable"

echo "release smoke scripts passed offline HTTPS, failure, checksum, backend, and archive fixture tests."
