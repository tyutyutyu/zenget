#!/usr/bin/env bash
# Integration tests for verify-release-tag.sh. Every repository, remote, and
# signing key is temporary; this script never creates refs in the production
# checkout.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
verifier="$repo_root/scripts/verify-release-tag.sh"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

work="$tmpdir/work"
origin="$tmpdir/origin.git"
gnupg_home="$tmpdir/gnupg"
mkdir -p "$gnupg_home"
chmod 700 "$gnupg_home"
export GNUPGHOME="$gnupg_home"

git init --bare "$origin" >/dev/null
git init "$work" >/dev/null
git -C "$work" config user.name "Zenget test signer"
git -C "$work" config user.email "zenget-test@example.invalid"
git -C "$work" config commit.gpgsign false
git -C "$work" remote add origin "$origin"
mkdir -p "$work/internal/version"
printf 'package version\n\nconst Version = "0.0.1"\n' >"$work/internal/version/version.go"
printf 'temporary release verifier test\n' >"$work/README.md"
git -C "$work" add .
git -C "$work" commit -m "test source" >/dev/null
git -C "$work" branch -M main
git -C "$work" push -u origin main >/dev/null
main_commit="$(git -C "$work" rev-parse HEAD)"

cat >"$tmpdir/key.batch" <<'KEY'
%no-protection
Key-Type: RSA
Key-Length: 2048
Name-Real: Zenget trusted test signer
Name-Email: trusted@example.invalid
Expire-Date: 0
%commit
KEY
gpg --batch --generate-key "$tmpdir/key.batch" >/dev/null 2>&1
trusted_fingerprint="$(gpg --batch --with-colons --list-secret-keys trusted@example.invalid | awk -F: '$1 == "fpr" { print toupper($10); exit }')"
if [[ -z "$trusted_fingerprint" ]]; then
  echo "could not create the trusted test key" >&2
  exit 1
fi
git -C "$work" config user.signingkey "$trusted_fingerprint"
export ZENGET_TRUSTED_GPG_FINGERPRINTS="$trusted_fingerprint"
trusted_public_keys="$(gpg --batch --armor --export "$trusted_fingerprint")"
export ZENGET_TRUSTED_GPG_PUBLIC_KEYS="$trusted_public_keys"

expect_success() {
  local label="$1"
  local tag="$2"
  local commit="$3"
  local output
  if ! output="$(cd "$work" && "$verifier" "$tag" "$commit" 2>&1)"; then
    printf '%s unexpectedly failed:\n%s\n' "$label" "$output" >&2
    exit 1
  fi
}

expect_failure() {
  local label="$1"
  local tag="$2"
  local commit="$3"
  local output
  if output="$(cd "$work" && "$verifier" "$tag" "$commit" 2>&1)"; then
    printf '%s unexpectedly passed:\n%s\n' "$label" "$output" >&2
    exit 1
  fi
}

git -C "$work" tag -s -a v0.0.1 -m "zenget v0.0.1" HEAD
git -C "$work" push origin v0.0.1 >/dev/null
expect_success "trusted signed annotated tag" v0.0.1 "$main_commit"

export ZENGET_TRUSTED_GPG_PUBLIC_KEYS=""
expect_failure "missing public key despite populated ambient keyring" v0.0.1 "$main_commit"
export ZENGET_TRUSTED_GPG_PUBLIC_KEYS="invalid public key"
expect_failure "malformed public key" v0.0.1 "$main_commit"
export ZENGET_TRUSTED_GPG_PUBLIC_KEYS="-----BEGIN PGP PRIVATE KEY BLOCK-----"
expect_failure "private key input" v0.0.1 "$main_commit"
export ZENGET_TRUSTED_GPG_PUBLIC_KEYS="$trusted_public_keys"
mkdir -m 700 "$tmpdir/empty-keyring"
GNUPGHOME="$tmpdir/empty-keyring" expect_success "fresh runner without ambient keys" v0.0.1 "$main_commit"

export ZENGET_TRUSTED_GPG_FINGERPRINTS="${trusted_fingerprint: -16}"
expect_failure "short key id is not a full trusted fingerprint" v0.0.1 "$main_commit"
export ZENGET_TRUSTED_GPG_FINGERPRINTS="$trusted_fingerprint"

git -C "$work" tag -a v0.0.2 -m "unsigned annotated tag" HEAD
expect_failure "unsigned annotated tag" v0.0.2 "$main_commit"

git -C "$work" tag v0.0.3 HEAD
expect_failure "lightweight tag" v0.0.3 "$main_commit"

fake_object="$(printf 'object %s\ntype commit\ntag v0.0.4\ntagger Test <test@example.invalid> 0 +0000\n\ngpgsig -----BEGIN PGP SIGNATURE-----\n fake\n -----END PGP SIGNATURE-----\n' "$main_commit" | git -C "$work" hash-object -w --stdin -t tag)"
git -C "$work" update-ref refs/tags/v0.0.4 "$fake_object"
expect_failure "forged gpgsig field" v0.0.4 "$main_commit"

git -C "$work" tag -s -a v0.0.5 -m "corrupt signature" HEAD
corrupt_object="$(git -C "$work" cat-file -p v0.0.5 | sed 's/BEGIN PGP SIGNATURE/BEGIN PGP SIGNATURX/' | git -C "$work" hash-object -w --stdin -t tag)"
git -C "$work" update-ref refs/tags/v0.0.5 "$corrupt_object"
expect_failure "corrupt signature" v0.0.5 "$main_commit"

cat >"$tmpdir/foreign.batch" <<'KEY'
%no-protection
Key-Type: RSA
Key-Length: 2048
Name-Real: Zenget foreign test signer
Name-Email: foreign@example.invalid
Expire-Date: 0
%commit
KEY
gpg --batch --generate-key "$tmpdir/foreign.batch" >/dev/null 2>&1
foreign_fingerprint="$(gpg --batch --with-colons --list-secret-keys foreign@example.invalid | awk -F: '$1 == "fpr" { print toupper($10); exit }')"
export ZENGET_TRUSTED_GPG_PUBLIC_KEYS="$(gpg --batch --armor --export "$trusted_fingerprint" "$foreign_fingerprint")"
git -C "$work" config user.signingkey "$foreign_fingerprint"
git -C "$work" tag -s -a v0.0.6 -m "foreign signature" HEAD
expect_failure "untrusted signer" v0.0.6 "$main_commit"
git -C "$work" config user.signingkey "$trusted_fingerprint"

git -C "$work" tag -s -a v0.0.9 -m "source mismatch" HEAD
expect_failure "source version mismatch" v0.0.9 "$main_commit"

git -C "$work" tag -s -a v0.0.8 -m "event mismatch" HEAD
expect_failure "event SHA mismatch" v0.0.8 "$(git -C "$work" rev-parse HEAD^ 2>/dev/null || printf '%040d' 0)"

git -C "$work" checkout --detach >/dev/null
printf 'outside main\n' >"$work/outside.txt"
git -C "$work" add outside.txt
git -C "$work" commit -m "outside main" >/dev/null
outside_commit="$(git -C "$work" rev-parse HEAD)"
git -C "$work" tag -s -a v0.0.7 -m "outside main" HEAD
expect_failure "commit outside origin/main" v0.0.7 "$outside_commit"
git -C "$work" checkout main >/dev/null

expect_failure "duplicate tag on one commit" v0.0.1 "$main_commit"

git -C "$work" remote set-url origin "$tmpdir/missing-origin.git"
expect_failure "verifier fetch failure" v0.0.1 "$main_commit"

echo "verify-release-tag.sh integration tests passed"
