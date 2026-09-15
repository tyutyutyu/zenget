package main

import (
	"path/filepath"
	"testing"
)

func TestVerifyOfflineFixture(t *testing.T) {
	root := "testdata"
	report, err := verifyOffline(
		filepath.Join(root, "cli-v2.100.0-checksums.txt"),
		filepath.Join(root, "cli-v2.100.0-release.bundle.jsonl"),
		filepath.Join(root, "github-trusted-root.json"),
	)
	if err != nil {
		t.Fatalf("verifyOffline() error = %v", err)
	}
	if report.ArtifactDigest != "6b5916dffcfa6f593b1db7890f2ddc485318e99fa263acf73aa28ebb877b53cd" {
		t.Fatalf("artifact digest = %q", report.ArtifactDigest)
	}
	if report.Predicate != expectedPredicate {
		t.Fatalf("predicate = %q", report.Predicate)
	}
}

func TestVerifyOfflineFixtureRejectsMissingArtifact(t *testing.T) {
	_, err := verifyOffline("testdata/missing", "testdata/cli-v2.100.0-release.bundle.jsonl", "testdata/github-trusted-root.json")
	if err == nil {
		t.Fatal("verifyOffline() error = nil, want missing artifact error")
	}
}
