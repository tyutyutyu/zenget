# Offline attestation harness

This nested module is a non-production spike fixture for TASK-0051. It keeps
the root `zenget` module and CLI free of experimental attestation dependencies
while measuring the embedded `sigstore-go` approach.

From this directory, the default fixture can be verified without network
access after the module and toolchain have been prepared:

```text
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go run .
```

The harness checks all of the following locally:

- the artifact's SHA-256 subject digest;
- the Sigstore DSSE signature and GitHub trusted root;
- the RFC 3161 observer timestamp; and
- the exact release-attestation predicate and GitHub release identity SAN.

The checked-in artifact is `gh_2.100.0_checksums.txt` from the public
`cli/cli` v2.100.0 release. The bundle was obtained with:

```text
gh release download v2.100.0 --repo cli/cli --pattern gh_2.100.0_checksums.txt
gh attestation download ./gh_2.100.0_checksums.txt --repo cli/cli
gh attestation trusted-root > trusted_root.jsonl
```

The bundle is the response for the artifact SHA-256 digest, and
`github-trusted-root.json` is the GitHub-specific line from the trusted-root
output. The fixture source is pinned by the release tag, API digest filename,
and the SHA-256 values listed below. If the fixture is refreshed, rerun the
commands above, copy only the public artifact/bundle and the GitHub root line,
recalculate the hashes, and update this document and the decision record in
the same change. Re-download the trusted root before accepting a new bundle;
do not silently reuse expired trust metadata.

Fixture hashes (checked 2026-09-07):

```text
6b5916dffcfa6f593b1db7890f2ddc485318e99fa263acf73aa28ebb877b53cd  testdata/cli-v2.100.0-checksums.txt
0d1f76b61591b6ac4f31f15eb0e2ef99fc3f09f22e1c381668d6a7de9ffe6322  testdata/cli-v2.100.0-release.bundle.jsonl
26b3382d5700afbcd84f980d1d5b6c52bff743dc2a8ee86b8b44c8e1245ce485  testdata/github-trusted-root.json
```

The harness intentionally has no network client. To demonstrate the offline
property, run the compiled binary or tests with network access disabled after
the initial `go mod download`/build step.
