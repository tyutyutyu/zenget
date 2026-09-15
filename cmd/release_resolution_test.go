package cmd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"zenget/internal/provider"
)

type resolutionTestProvider struct {
	latest       *provider.Release
	byTag        *provider.Release
	byTagErr     error
	listed       []provider.Release
	latestCalls  int
	byTagCalls   int
	listCalls    int
	requestedTag string
}

func (p *resolutionTestProvider) LatestRelease(context.Context, string, string) (*provider.Release, error) {
	p.latestCalls++
	return p.latest, nil
}

func (p *resolutionTestProvider) ReleaseByTag(_ context.Context, _, _, tag string) (*provider.Release, error) {
	p.byTagCalls++
	p.requestedTag = tag
	return p.byTag, p.byTagErr
}

func (p *resolutionTestProvider) ListReleases(context.Context, string, string) ([]provider.Release, error) {
	p.listCalls++
	return p.listed, nil
}

func (p *resolutionTestProvider) DownloadAsset(context.Context, provider.Asset, string, ...int64) error {
	return fmt.Errorf("release resolution tests do not download assets")
}

func TestResolveInstallReleaseUsesSharedSelectionPolicy(t *testing.T) {
	now := time.Now().UTC()
	old := &provider.Release{TagName: "v1.0.0", PublishedAt: now.AddDate(0, 0, -14)}
	young := &provider.Release{TagName: "v2.0.0", PublishedAt: now.AddDate(0, 0, -1)}

	tests := []struct {
		name       string
		options    installOptions
		provider   *resolutionTestProvider
		want       *provider.Release
		wantAge    bool
		wantLatest int
		wantTag    int
		wantList   int
	}{
		{
			name:       "injected release",
			options:    installOptions{release: old},
			provider:   &resolutionTestProvider{latest: young},
			want:       old,
			wantLatest: 0,
			wantTag:    0,
			wantList:   0,
		},
		{
			name:       "latest release",
			options:    installOptions{},
			provider:   &resolutionTestProvider{latest: young},
			want:       young,
			wantLatest: 1,
			wantTag:    0,
			wantList:   0,
		},
		{
			name:       "exact tag",
			options:    installOptions{tag: "v1.0.0", exactTag: true},
			provider:   &resolutionTestProvider{byTag: old},
			want:       old,
			wantLatest: 0,
			wantTag:    1,
			wantList:   0,
		},
		{
			name:     "tag with substring fallback",
			options:  installOptions{tag: "1.0"},
			provider: &resolutionTestProvider{byTagErr: errors.New("release lookup returned 404"), listed: []provider.Release{*young, *old}},
			want:     old,
			wantTag:  1,
			wantList: 1,
		},
		{
			name:       "minimum age",
			options:    installOptions{minAgeDays: 7},
			provider:   &resolutionTestProvider{listed: []provider.Release{*young, *old}},
			want:       old,
			wantAge:    true,
			wantLatest: 0,
			wantTag:    0,
			wantList:   1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ageFiltered, err := resolveInstallRelease(context.Background(), test.provider, "acme", "widget", test.options)
			if err != nil {
				t.Fatalf("resolveInstallRelease() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("resolveInstallRelease() = %#v, want %#v", got, test.want)
			}
			if ageFiltered != test.wantAge {
				t.Errorf("ageFiltered = %v, want %v", ageFiltered, test.wantAge)
			}
			if test.provider.latestCalls != test.wantLatest {
				t.Errorf("LatestRelease calls = %d, want %d", test.provider.latestCalls, test.wantLatest)
			}
			if test.provider.byTagCalls != test.wantTag {
				t.Errorf("ReleaseByTag calls = %d, want %d", test.provider.byTagCalls, test.wantTag)
			}
			if test.provider.listCalls != test.wantList {
				t.Errorf("ListReleases calls = %d, want %d", test.provider.listCalls, test.wantList)
			}
		})
	}
}
