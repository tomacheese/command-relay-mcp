package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeVersion(t *testing.T) {
	if got := normalizeVersion("v1.2.3"); got != "1.2.3" {
		t.Fatalf("normalizeVersion(v1.2.3) = %q, want 1.2.3", got)
	}
	if got := normalizeVersion("1.2.3"); got != "1.2.3" {
		t.Fatalf("normalizeVersion(1.2.3) = %q, want 1.2.3", got)
	}
}

func TestUpdateNeeded(t *testing.T) {
	cases := []struct {
		current, tag string
		want         bool
	}{
		{"1.2.3", "v1.2.3", false},
		{"1.2.3", "v1.2.4", true},
		{"1.2.4", "v1.2.3", true},
	}
	for _, c := range cases {
		if got := updateNeeded(c.current, c.tag); got != c.want {
			t.Errorf("updateNeeded(%q, %q) = %v, want %v", c.current, c.tag, got, c.want)
		}
	}
}

func TestArchiveAssetName(t *testing.T) {
	cases := []struct {
		goarch  string
		want    string
		wantErr bool
	}{
		{"amd64", "command-relay-agent_Linux_x86_64.tar.gz", false},
		{"arm64", "command-relay-agent_Linux_arm64.tar.gz", false},
		{"386", "", true},
	}
	for _, c := range cases {
		got, err := archiveAssetName(c.goarch)
		if c.wantErr {
			if err == nil {
				t.Errorf("archiveAssetName(%q): want error, got nil", c.goarch)
			}
			continue
		}
		if err != nil {
			t.Errorf("archiveAssetName(%q): unexpected error: %v", c.goarch, err)
		}
		if got != c.want {
			t.Errorf("archiveAssetName(%q) = %q, want %q", c.goarch, got, c.want)
		}
	}
}

func TestSelectAsset(t *testing.T) {
	rel := &release{Assets: []asset{
		{Name: "command-relay-agent_Linux_x86_64.tar.gz", BrowserDownloadURL: "https://example.test/archive"},
		{Name: "command-relay-agent_1.2.3_checksums.txt", BrowserDownloadURL: "https://example.test/checksums"},
	}}

	a, err := selectAsset(rel, "command-relay-agent_Linux_x86_64.tar.gz")
	if err != nil {
		t.Fatalf("selectAsset: %v", err)
	}
	if a.BrowserDownloadURL != "https://example.test/archive" {
		t.Errorf("selectAsset URL = %q", a.BrowserDownloadURL)
	}

	if _, err := selectAsset(rel, "does-not-exist.tar.gz"); err == nil {
		t.Error("selectAsset: want error for missing asset, got nil")
	}

	c, err := selectChecksumsAsset(rel)
	if err != nil {
		t.Fatalf("selectChecksumsAsset: %v", err)
	}
	if c.BrowserDownloadURL != "https://example.test/checksums" {
		t.Errorf("selectChecksumsAsset URL = %q", c.BrowserDownloadURL)
	}
}

func TestSelectChecksumsAsset_Missing(t *testing.T) {
	rel := &release{Assets: []asset{{Name: "command-relay-agent_Linux_x86_64.tar.gz"}}}
	if _, err := selectChecksumsAsset(rel); err == nil {
		t.Error("selectChecksumsAsset: want error when no checksums asset present, got nil")
	}
}

func TestFetchLatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"v1.2.3","assets":[{"name":"a.tar.gz","browser_download_url":"https://example.test/a.tar.gz"}]}`))
	}))
	defer srv.Close()

	rel, err := fetchLatestRelease(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want v1.2.3", rel.TagName)
	}
	if len(rel.Assets) != 1 || rel.Assets[0].Name != "a.tar.gz" {
		t.Errorf("Assets = %+v", rel.Assets)
	}
}

func TestFetchLatestRelease_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := fetchLatestRelease(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Error("fetchLatestRelease: want error on 503, got nil")
	}
}
