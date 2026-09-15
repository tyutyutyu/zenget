// Command attestation-harness verifies a checked-in Sigstore bundle offline.
//
// This is intentionally a nested, non-production Go module. It exists to
// measure the embedded sigstore-go approach and to keep the zenget CLI free of
// experimental attestation dependencies while the architecture is undecided.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	defaultArtifact    = "testdata/cli-v2.100.0-checksums.txt"
	defaultBundle      = "testdata/cli-v2.100.0-release.bundle.jsonl"
	defaultTrustedRoot = "testdata/github-trusted-root.json"
	expectedPredicate  = "https://in-toto.io/attestation/release/v0.2"
	expectedSAN        = "https://dotcom.releases.github.com"
)

type verificationReport struct {
	ArtifactDigest string
	Bundle         string
	Predicate      string
}

func main() {
	artifact := flag.String("artifact", defaultArtifact, "artifact to verify")
	bundlePath := flag.String("bundle", defaultBundle, "Sigstore bundle to verify")
	trustedRoot := flag.String("trusted-root", defaultTrustedRoot, "trusted root JSON to use")
	flag.Parse()

	if flag.NArg() != 0 {
		fail("unexpected positional arguments: %v", flag.Args())
	}

	report, err := verifyOffline(*artifact, *bundlePath, *trustedRoot)
	if err != nil {
		fail("offline verification failed: %v", err)
	}

	fmt.Printf("verified predicate=%s artifact_sha256=%s bundle=%s\n",
		report.Predicate, report.ArtifactDigest, report.Bundle)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func verifyOffline(artifactPath, bundlePath, trustedRootPath string) (verificationReport, error) {
	if artifactPath == "" || bundlePath == "" || trustedRootPath == "" {
		return verificationReport{}, errors.New("artifact, bundle, and trusted root paths are required")
	}

	attestation, err := bundle.LoadJSONFromPath(bundlePath)
	if err != nil {
		return verificationReport{}, fmt.Errorf("load bundle %q: %w", bundlePath, err)
	}

	trustedRoot, err := root.NewTrustedRootFromPath(trustedRootPath)
	if err != nil {
		return verificationReport{}, fmt.Errorf("load trusted root %q: %w", trustedRootPath, err)
	}

	identity, err := verify.NewShortCertificateIdentity("", "^$", expectedSAN, "")
	if err != nil {
		return verificationReport{}, fmt.Errorf("configure certificate identity: %w", err)
	}

	verifier, err := verify.NewVerifier(trustedRoot, verify.WithObserverTimestamps(1))
	if err != nil {
		return verificationReport{}, fmt.Errorf("configure verifier: %w", err)
	}

	artifactFile, err := os.Open(filepath.Clean(artifactPath))
	if err != nil {
		return verificationReport{}, fmt.Errorf("open artifact %q: %w", artifactPath, err)
	}
	defer artifactFile.Close()

	result, err := verifier.Verify(attestation, verify.NewPolicy(
		verify.WithArtifact(artifactFile),
		verify.WithCertificateIdentity(identity),
	))
	if err != nil {
		return verificationReport{}, err
	}
	if result.Statement == nil {
		return verificationReport{}, errors.New("verification produced no in-toto statement")
	}
	if result.Statement.PredicateType != expectedPredicate {
		return verificationReport{}, fmt.Errorf(
			"unexpected predicate type %q, expected %q",
			result.Statement.PredicateType,
			expectedPredicate,
		)
	}

	digest, err := digestFile(artifactFile)
	if err != nil {
		return verificationReport{}, fmt.Errorf("calculate artifact digest: %w", err)
	}

	return verificationReport{
		ArtifactDigest: digest,
		Bundle:         bundlePath,
		Predicate:      result.Statement.PredicateType,
	}, nil
}

func digestFile(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
