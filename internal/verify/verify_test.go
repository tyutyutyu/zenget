package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/limits"
)

func TestParseSidecarChecksumFileBareDigest(t *testing.T) {
	content := []byte("  e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  \n")
	got, ok := parseSidecarChecksumFile(content)
	if !ok {
		t.Fatal("expected bare digest to be parsed")
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSHA256FileExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("artifact"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("artifact"))
	want := hex.EncodeToString(digest[:])
	if got != want {
		t.Fatalf("SHA256File() = %q, want %q", got, want)
	}
}

func TestVerifyWithChecksumAsset(t *testing.T) {
	assetData := []byte("recipe payload")
	assetDigest := sha256.Sum256(assetData)
	digest := hex.EncodeToString(assetDigest[:])
	checksums := map[string][]byte{
		"/raw":       []byte(digest + "\n"),
		"/sha256sum": []byte(digest + "  widget.tar.gz\n"),
		"/mismatch":  []byte(strings.Repeat("0", 64) + "\n"),
		"/malformed": []byte("not a digest\n"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, ok := checksums[r.URL.Path]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	asset := github.Asset{Name: "widget.tar.gz"}
	assetPath := writeTempFile(t, assetData)

	for _, test := range []struct {
		name   string
		path   string
		format ChecksumFormat
	}{
		{name: "raw", path: "/raw", format: ChecksumFormatRaw},
		{name: "sha256sum", path: "/sha256sum", format: ChecksumFormatSHA256Sum},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			checksumAsset := github.Asset{Name: "recipe-checksum", BrowserDownloadURL: server.URL + test.path}
			if err := VerifyWithChecksumAsset(context.Background(), client, &output, asset, checksumAsset, test.format, assetPath); err != nil {
				t.Fatalf("VerifyWithChecksumAsset() error = %v", err)
			}
			if !strings.Contains(output.String(), "recipe-checksum") {
				t.Fatalf("verification output = %q", output.String())
			}
		})
	}

	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{name: "mismatch", path: "/mismatch", want: "checksum mismatch"},
		{name: "malformed", path: "/malformed", want: "does not contain a valid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checksumAsset := github.Asset{Name: test.name, BrowserDownloadURL: server.URL + test.path}
			err := VerifyWithChecksumAsset(context.Background(), client, io.Discard, asset, checksumAsset, ChecksumFormatRaw, assetPath)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyWithChecksumAsset() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseSidecarChecksumFileStandardLine(t *testing.T) {
	content := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  widget.tar.gz\n")
	got, ok := parseSidecarChecksumFile(content)
	if !ok {
		t.Fatal("expected digest to be found")
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseSidecarChecksumFileRejectsWrongTarget(t *testing.T) {
	content := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  other.tar.gz\n")
	if _, ok := parseSidecarChecksumFile(content, "widget.tar.gz"); ok {
		t.Fatal("expected wrong sidecar target to be rejected")
	}
}

func TestParseSidecarChecksumFileMalformedLine(t *testing.T) {
	content := []byte("not-a-hex-digest\n")
	if _, ok := parseSidecarChecksumFile(content); ok {
		t.Fatal("expected malformed line to be skipped")
	}
}

func TestParseAggregateChecksumFileStandardLine(t *testing.T) {
	content := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  widget.tar.gz\n")
	got, ok := parseAggregateChecksumFile(content, "widget.tar.gz")
	if !ok {
		t.Fatal("expected digest to be found")
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseAggregateChecksumFileBinaryMarker(t *testing.T) {
	content := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 *widget.tar.gz\n")
	got, ok := parseAggregateChecksumFile(content, "widget.tar.gz")
	if !ok {
		t.Fatal("expected digest with binary marker to be found")
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseAggregateChecksumFileCaseInsensitiveHex(t *testing.T) {
	content := []byte("E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855  widget.tar.gz\n")
	got, ok := parseAggregateChecksumFile(content, "widget.tar.gz")
	if !ok {
		t.Fatal("expected case-insensitive digest to be found")
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseAggregateChecksumFileRelativePath(t *testing.T) {
	content := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  ./dist/widget.tar.gz\n")
	got, ok := parseAggregateChecksumFile(content, "widget.tar.gz")
	if !ok {
		t.Fatal("expected relative path to match basename")
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseAggregateChecksumFileMissingEntry(t *testing.T) {
	content := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  other.tar.gz\n")
	if _, ok := parseAggregateChecksumFile(content, "widget.tar.gz"); ok {
		t.Fatal("expected missing entry to return false")
	}
}

func TestParseAggregateChecksumFileIgnoresBareDigest(t *testing.T) {
	content := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n")
	if _, ok := parseAggregateChecksumFile(content, "widget.tar.gz"); ok {
		t.Fatal("expected bare digest in aggregate file to be ignored")
	}
}

func TestParseAggregateChecksumFileIgnoresCommentsAndEmptyLines(t *testing.T) {
	content := []byte("\n# generated by sha256sum\n\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  widget.tar.gz\n\n")
	got, ok := parseAggregateChecksumFile(content, "widget.tar.gz")
	if !ok {
		t.Fatal("expected digest to be found despite comments and blanks")
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestIsAggregateFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"sha256.sum", true},
		{"SHA256.SUM", true},
		{"checksums.txt", true},
		{"Checksums.TXT", true},
		{"SHA256SUMS", true},
		{"sha256sums", true},
		{"checksums.sha256", true},
		{"CHECKSUMS.SHA256", true},
		{"widget.tar.gz", false},
		{"widget.tar.gz.sha256", false},
		{"sha256", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAggregateFile(tc.name); got != tc.want {
				t.Fatalf("isAggregateFile(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsHexDigest(t *testing.T) {
	valid := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if !isHexDigest(valid) {
		t.Fatalf("expected %q to be a valid hex digest", valid)
	}
	invalid := []string{
		"",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85g",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85 ",
	}
	for _, v := range invalid {
		if isHexDigest(v) {
			t.Fatalf("expected %q to be invalid", v)
		}
	}
}

func TestParseGitHubDigest(t *testing.T) {
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "lowercase", input: "sha256:" + digest, want: digest, ok: true},
		{name: "case insensitive", input: "SHA256:" + strings.ToUpper(digest), want: digest, ok: true},
		{name: "missing prefix", input: digest, ok: false},
		{name: "wrong algorithm", input: "sha512:" + digest, ok: false},
		{name: "short digest", input: "sha256:" + digest[:63], ok: false},
		{name: "non hexadecimal", input: "sha256:" + strings.Repeat("g", 64), ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseGitHubDigest(test.input)
			if test.ok {
				if err != nil {
					t.Fatalf("parseGitHubDigest() error = %v", err)
				}
				if got != test.want {
					t.Fatalf("parseGitHubDigest() = %q, want %q", got, test.want)
				}
				return
			}
			if err == nil {
				t.Fatal("parseGitHubDigest() error = nil, want invalid digest error")
			}
			if !strings.Contains(err.Error(), "expected sha256:<64 hex>") {
				t.Fatalf("parseGitHubDigest() error = %q, want format message", err)
			}
		})
	}
}

func TestFilepathBase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"widget.tar.gz", "widget.tar.gz"},
		{"./dist/widget.tar.gz", "widget.tar.gz"},
		{"dist/widget.tar.gz", "widget.tar.gz"},
		{"/abs/path/widget.tar.gz", "widget.tar.gz"},
		{"widget.tar.gz/", "widget.tar.gz"},
	}
	for _, tc := range cases {
		if got := filepathBase(tc.in); got != tc.want {
			t.Fatalf("filepathBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestVerifySidecarChecksumMatch(t *testing.T) {
	assetData := []byte("payload")
	assetHash := sha256.Sum256(assetData)
	checksum := strings.ToLower(hex.EncodeToString(assetHash[:])) + "\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/assets/widget.tar.gz.sha256" {
			_, _ = w.Write([]byte(checksum))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	assetPath := writeTempFile(t, assetData)
	client := github.New()
	client.BaseURL = server.URL
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "widget.tar.gz", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz"},
			{Name: "widget.tar.gz.sha256", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz.sha256"},
		},
	}
	asset := release.Assets[0]

	var out bytes.Buffer
	if err := Verify(context.Background(), client, &out, asset, release, assetPath); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out.String(), "Verified SHA-256 checksum for widget.tar.gz") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestVerifyRejectsOversizeChecksumAsset(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := (&config.Config{MaxChecksumBytes: 4}).Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer is not flushable")
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = io.WriteString(w, "12345")
	}))
	defer server.Close()

	assetPath := writeTempFile(t, []byte("payload"))
	release := &github.Release{Assets: []github.Asset{
		{Name: "widget.tar.gz", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz"},
		{Name: "widget.tar.gz.sha256", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz.sha256"},
	}}
	err := Verify(context.Background(), github.New(), io.Discard, release.Assets[0], release, assetPath)
	if err == nil || !errors.Is(err, limits.ErrLimitExceeded) {
		t.Fatalf("Verify() error = %v, want checksum size limit", err)
	}
	if !strings.Contains(err.Error(), "checksum asset limit=4") {
		t.Fatalf("Verify() error = %v, want bounded checksum download", err)
	}
}

func TestVerifyAggregateChecksumMatch(t *testing.T) {
	assetData := []byte("payload")
	assetHash := sha256.Sum256(assetData)
	checksums := fmt.Sprintf("%s  widget.tar.gz\n", strings.ToLower(hex.EncodeToString(assetHash[:])))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/assets/checksums.txt" {
			_, _ = w.Write([]byte(checksums))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	assetPath := writeTempFile(t, assetData)
	client := github.New()
	client.BaseURL = server.URL
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "widget.tar.gz", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz"},
			{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/assets/checksums.txt"},
		},
	}
	asset := release.Assets[0]

	var out bytes.Buffer
	if err := Verify(context.Background(), client, &out, asset, release, assetPath); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out.String(), "Verified SHA-256 checksum for widget.tar.gz") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestVerifyChecksumMismatch(t *testing.T) {
	assetData := []byte("payload")
	checksum := strings.Repeat("0", 64) + "\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/assets/widget.tar.gz.sha256" {
			_, _ = w.Write([]byte(checksum))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	assetPath := writeTempFile(t, assetData)
	client := github.New()
	client.BaseURL = server.URL
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "widget.tar.gz", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz"},
			{Name: "widget.tar.gz.sha256", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz.sha256"},
		},
	}

	if err := Verify(context.Background(), client, io.Discard, release.Assets[0], release, assetPath); err == nil {
		t.Fatal("expected checksum mismatch error")
	} else if !strings.Contains(err.Error(), "checksum mismatch for \"widget.tar.gz\"") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyNoChecksumProceeds(t *testing.T) {
	assetData := []byte("payload")
	assetPath := writeTempFile(t, assetData)
	client := github.New()
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "widget.tar.gz", BrowserDownloadURL: "http://example.com/assets/widget.tar.gz"},
		},
	}

	if err := Verify(context.Background(), client, io.Discard, release.Assets[0], release, assetPath); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyWithPolicyRequiredRejectsMissingChecksumBeforeReadingAsset(t *testing.T) {
	release := &github.Release{TagName: "v1.0.0", Assets: []github.Asset{{Name: "widget.tar.gz"}}}
	assetPath := filepath.Join(t.TempDir(), "missing-asset")
	err := VerifyWithPolicy(
		context.Background(),
		github.New(),
		io.Discard,
		release.Assets[0],
		release,
		assetPath,
		"acme/widget",
		config.ChecksumPolicyRequired,
	)
	if err == nil {
		t.Fatal("VerifyWithPolicy() error = nil, want missing checksum error")
	}
	for _, want := range []string{
		"policy=required",
		"repository=acme/widget",
		"release=v1.0.0",
		`asset="widget.tar.gz"`,
		"no valid SHA-256 checksum",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("VerifyWithPolicy() error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "compute SHA-256") {
		t.Fatalf("VerifyWithPolicy() read the asset before rejecting missing checksum: %v", err)
	}
}

func TestVerifyWithPolicyReportsEffectivePolicyOnSuccess(t *testing.T) {
	assetData := []byte("payload")
	digest := sha256.Sum256(assetData)
	digestText := hex.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/assets/widget.tar.gz.sha256" {
			_, _ = io.WriteString(w, digestText+"\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	release := &github.Release{TagName: "v1.0.0", Assets: []github.Asset{
		{Name: "widget.tar.gz"},
		{Name: "widget.tar.gz.sha256", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz.sha256"},
	}}
	var out bytes.Buffer
	err := VerifyWithPolicy(
		context.Background(),
		github.New(),
		&out,
		release.Assets[0],
		release,
		writeTempFile(t, assetData),
		"acme/widget",
		config.ChecksumPolicyRequired,
	)
	if err != nil {
		t.Fatalf("VerifyWithPolicy() error = %v", err)
	}
	for _, want := range []string{"policy=required", "repository=acme/widget", "release=v1.0.0"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("verification output = %q, want %q", out.String(), want)
		}
	}
}

func TestVerifyWithPolicyRejectsInvalidPolicy(t *testing.T) {
	release := &github.Release{TagName: "v1.0.0", Assets: []github.Asset{{Name: "widget.tar.gz"}}}
	err := VerifyWithPolicy(context.Background(), github.New(), io.Discard, release.Assets[0], release, "missing", "acme/widget", "off")
	if err == nil || !strings.Contains(err.Error(), "invalid checksum policy") {
		t.Fatalf("VerifyWithPolicy() error = %v, want invalid policy error", err)
	}
}

func TestVerifyGitHubDigestMatch(t *testing.T) {
	assetData := []byte("payload")
	digest := sha256.Sum256(assetData)
	digestText := strings.ToUpper(hex.EncodeToString(digest[:]))
	assetPath := writeTempFile(t, assetData)
	release := &github.Release{Assets: []github.Asset{{
		Name:   "widget.tar.gz",
		Digest: "SHA256:" + digestText,
	}}}

	var out bytes.Buffer
	if err := Verify(context.Background(), github.New(), &out, release.Assets[0], release, assetPath); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out.String(), "Verified SHA-256 checksum for widget.tar.gz") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestVerifyGitHubDigestMismatch(t *testing.T) {
	assetPath := writeTempFile(t, []byte("payload"))
	release := &github.Release{Assets: []github.Asset{{
		Name:   "widget.tar.gz",
		Digest: "sha256:" + strings.Repeat("0", 64),
	}}}

	err := Verify(context.Background(), github.New(), io.Discard, release.Assets[0], release, assetPath)
	if err == nil {
		t.Fatal("Verify() error = nil, want GitHub digest mismatch")
	}
	if !strings.Contains(err.Error(), "from GitHub digest") {
		t.Fatalf("Verify() error = %q, want GitHub digest source", err)
	}
}

func TestVerifyRejectsInvalidGitHubDigestBeforeReadingAsset(t *testing.T) {
	release := &github.Release{Assets: []github.Asset{{
		Name:   "widget.tar.gz",
		Digest: "sha512:" + strings.Repeat("0", 64),
	}}}

	err := Verify(context.Background(), github.New(), io.Discard, release.Assets[0], release, filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("Verify() error = nil, want invalid GitHub digest")
	}
	if !strings.Contains(err.Error(), "invalid digest") || !strings.Contains(err.Error(), "expected sha256:<64 hex>") {
		t.Fatalf("Verify() error = %q, want invalid digest details", err)
	}
	if strings.Contains(err.Error(), "compute SHA-256") {
		t.Fatalf("Verify() read the asset before rejecting the digest: %v", err)
	}
}

func TestVerifyChecksGitHubSidecarAndAggregateDigests(t *testing.T) {
	const token = "private-test-token"
	assetData := []byte("payload")
	digest := sha256.Sum256(assetData)
	digestText := strings.ToLower(hex.EncodeToString(digest[:]))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q, want Bearer token", got)
		}
		if got := r.Header.Get("Accept"); got != "application/octet-stream" {
			t.Errorf("Accept header = %q, want application/octet-stream", got)
		}
		switch r.URL.Path {
		case "/api/assets/widget.tar.gz.sha256":
			_, _ = io.WriteString(w, digestText+"\n")
		case "/api/assets/checksums.txt":
			_, _ = io.WriteString(w, digestText+"  widget.tar.gz\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	assetPath := writeTempFile(t, assetData)
	client := github.New()
	client.BaseURL = server.URL
	client.Token = token
	release := &github.Release{Assets: []github.Asset{
		{
			Name:   "widget.tar.gz",
			Digest: "sha256:" + digestText,
		},
		{
			Name:               "widget.tar.gz.sha256",
			URL:                server.URL + "/api/assets/widget.tar.gz.sha256",
			BrowserDownloadURL: server.URL + "/browser/widget.tar.gz.sha256",
		},
		{
			Name:               "checksums.txt",
			URL:                server.URL + "/api/assets/checksums.txt",
			BrowserDownloadURL: server.URL + "/browser/checksums.txt",
		},
	}}

	var out bytes.Buffer
	if err := Verify(context.Background(), client, &out, release.Assets[0], release, assetPath); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out.String(), "Verified SHA-256 checksum for widget.tar.gz") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestVerifyReportsAggregateMismatchAlongsideGitHubDigest(t *testing.T) {
	assetData := []byte("payload")
	digest := sha256.Sum256(assetData)
	digestText := strings.ToLower(hex.EncodeToString(digest[:]))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assets/widget.tar.gz.sha256":
			_, _ = io.WriteString(w, digestText+"\n")
		case "/assets/checksums.txt":
			_, _ = io.WriteString(w, strings.Repeat("0", 64)+"  widget.tar.gz\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	assetPath := writeTempFile(t, assetData)
	client := github.New()
	client.BaseURL = server.URL
	release := &github.Release{Assets: []github.Asset{
		{Name: "widget.tar.gz", Digest: "sha256:" + digestText},
		{Name: "widget.tar.gz.sha256", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz.sha256"},
		{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/assets/checksums.txt"},
	}}

	err := Verify(context.Background(), client, io.Discard, release.Assets[0], release, assetPath)
	if err == nil {
		t.Fatal("Verify() error = nil, want aggregate mismatch")
	}
	if !strings.Contains(err.Error(), "from checksums.txt") {
		t.Fatalf("Verify() error = %q, want aggregate source", err)
	}
}

func TestVerifyNilRelease(t *testing.T) {
	client := github.New()
	if err := Verify(context.Background(), client, io.Discard, github.Asset{Name: "widget.tar.gz"}, nil, "ignored"); err == nil {
		t.Fatal("expected error for nil release")
	}
}

func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "asset")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return tmp
}

func TestFindExpectedDigestSidecarPrecedence(t *testing.T) {
	assetData := []byte("payload")
	assetHash := sha256.Sum256(assetData)
	goodChecksum := strings.ToLower(hex.EncodeToString(assetHash[:])) + "\n"
	badChecksum := strings.Repeat("0", 64) + "\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assets/widget.tar.gz.sha256":
			_, _ = w.Write([]byte(badChecksum))
		case "/assets/checksums.txt":
			_, _ = w.Write([]byte(goodChecksum[:len(goodChecksum)-1] + "  widget.tar.gz\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "widget.tar.gz", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz"},
			{Name: "widget.tar.gz.sha256", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz.sha256"},
			{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/assets/checksums.txt"},
		},
	}

	expected, source, ok, err := findExpectedDigest(context.Background(), client, release.Assets[0], release)
	if err != nil {
		t.Fatalf("findExpectedDigest: %v", err)
	}
	if !ok {
		t.Fatal("expected digest to be found")
	}
	if source != "widget.tar.gz.sha256" {
		t.Fatalf("source = %q, want sidecar file", source)
	}
	if expected != badChecksum[:64] {
		t.Fatalf("expected sidecar digest, got %q", expected)
	}
}

func TestVerifyMalformedAggregateFallsBack(t *testing.T) {
	assetData := []byte("payload")
	assetPath := writeTempFile(t, assetData)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/assets/checksums.txt" {
			_, _ = w.Write([]byte("not-a-hex-digest  widget.tar.gz\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "widget.tar.gz", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz"},
			{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/assets/checksums.txt"},
		},
	}

	err := Verify(context.Background(), client, io.Discard, release.Assets[0], release, assetPath)
	if err == nil {
		t.Fatal("verify should reject malformed discovered checksum")
	}
	if !strings.Contains(err.Error(), "checksums.txt") {
		t.Fatalf("Verify() error = %q, want checksum source", err)
	}
}
