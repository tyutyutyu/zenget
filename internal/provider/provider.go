// Package provider defines the forge-neutral release provider contract used
// by zenget's command layer.
package provider

import (
	"context"
	"time"
)

// Asset describes a downloadable file attached to a release.
type Asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

// Release describes a published release for a repository.
type Release struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Provider resolves releases and downloads their assets. The optional
// DownloadAsset limit is a per-call override for the provider's configured
// release-download budget.
type Provider interface {
	LatestRelease(ctx context.Context, org, repo string) (*Release, error)
	ReleaseByTag(ctx context.Context, org, repo, tag string) (*Release, error)
	ListReleases(ctx context.Context, org, repo string) ([]Release, error)
	DownloadAsset(ctx context.Context, asset Asset, destPath string, maxBytes ...int64) error
}
