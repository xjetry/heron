package theme

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type githubRoundTripFunc func(*http.Request) (*http.Response, error)

func (f githubRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func githubResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r, ContentLength: int64(len(body))}
}

func requireGitHubError(t *testing.T, err error, kind string, contains string) {
	t.Helper()
	var target *GitHubError
	if !errors.As(err, &target) || target.Kind != kind || !strings.Contains(target.Error(), contains) {
		t.Fatalf("error = %#v; want GitHubError %q containing %q", err, kind, contains)
	}
}

func TestGitHubReleases(t *testing.T) {
	for _, input := range []string{"Example/Theme", "https://github.com/Example/Theme", "https://github.com/Example/Theme/"} {
		t.Run(input, func(t *testing.T) {
			calls := 0
			client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "https://api.github.com/repos/example/theme/releases?per_page=30&page=1" || r.Header.Get("Accept") != "application/vnd.github+json" {
					t.Fatalf("unexpected API request: %s; accept %q", r.URL, r.Header.Get("Accept"))
				}
				return githubResponse(r, 200, `[{"name":"Theme Two","tag_name":"v2","prerelease":true,"assets":[{"id":7,"name":"theme.ZIP","size":42},{"id":8,"name":"checksums.txt","size":12}]},{"name":"Draft","tag_name":"draft","draft":true,"assets":[]}]`), nil
			}))
			got, err := client.Releases(context.Background(), input)
			if err != nil || calls != 1 || len(got) != 1 || got[0].Name != "Theme Two" || got[0].Tag != "v2" || !got[0].Prerelease || len(got[0].Assets) != 1 || got[0].Assets[0] != (GitHubAsset{ID: 7, Name: "theme.ZIP", Size: 42}) {
				t.Fatalf("releases = %#v, calls = %d, err = %v", got, calls, err)
			}
		})
	}
}

func TestGitHubReleaseURLPreservesTag(t *testing.T) {
	for _, input := range []string{"https://github.com/owner/repo/releases/tag/release%2Fv1%2Btest", "https://github.com/owner/repo/releases/tag/release/v1%2Btest"} {
		t.Run(input, func(t *testing.T) {
			client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.EscapedPath() != "/repos/owner/repo/releases/tags/release%2Fv1+test" {
					t.Fatalf("tag was not kept as one escaped URL segment: %s", r.URL)
				}
				return githubResponse(r, 200, `{"name":"Release","tag_name":"release/v1+test","assets":[]}`), nil
			}))
			got, err := client.Releases(context.Background(), input)
			if err != nil || len(got) != 1 || got[0].Tag != "release/v1+test" {
				t.Fatalf("releases = %#v, err = %v", got, err)
			}
		})
	}
}

func TestGitHubRejectsInvalidSourceBeforeNetwork(t *testing.T) {
	inputs := []string{
		"", " owner/repo", "owner/repo ", "owner", "owner/repo/extra", "owner/..", "../repo",
		"http://github.com/owner/repo", "https://github.com.evil.test/owner/repo", "https://api.github.com/owner/repo",
		"https://user@github.com/owner/repo", "https://github.com:443/owner/repo", "https://github.com/owner/repo?x=y",
		"https://github.com/owner/repo?", "https://github.com/owner/repo#x", "https://github.com/owner/repo/releases",
		"https://github.com/owner/repo/releases/tag/", "https://github.com/owner/repo/releases/tag/%00",
		"https://github.com/owner/repo/releases/tag/v1/",
		"https://github.com/owner%2frepo/other", "https://github.com/owner/repo/archive/refs/tags/v1.zip",
		"https://github.com/owner/repo/releases/tag/%2e%2e", "https://github.com/owner/repo/releases/tag/a%3fb",
	}
	client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("invalid input reached network: %s", r.URL)
		return nil, nil
	}))
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			_, err := client.Releases(context.Background(), input)
			requireGitHubError(t, err, "invalid_argument", "repository")
		})
	}
	for _, tc := range []struct {
		repository string
		tag        string
		id         int64
	}{
		{"owner/repo", "", 7}, {"owner/repo", "v1", 0}, {"owner/repo", "v1", -1},
		{"owner/repo", "v1\n", 7}, {"owner/repo", "../v1", 7}, {"owner/repo", "a?b", 7},
		{"https://github.com/owner/repo/releases/tag/v2", "v1", 7},
	} {
		_, _, err := client.Download(context.Background(), tc.repository, tc.tag, tc.id)
		requireGitHubError(t, err, "invalid_argument", "")
	}
}

func TestGitHubDownloadExactAsset(t *testing.T) {
	var requests []string
	client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		switch len(requests) {
		case 1:
			if r.URL.String() != "https://api.github.com/repos/owner/repo/releases/tags/v1" {
				t.Fatalf("unexpected release lookup: %s", r.URL)
			}
			return githubResponse(r, 200, `{"name":"One","tag_name":"v1","assets":[{"id":6,"name":"other.zip","size":3},{"id":7,"name":"theme.zip","size":3,"browser_download_url":"http://127.0.0.1/ignored"}]}`), nil
		case 2:
			if r.URL.String() != "https://api.github.com/repos/owner/repo/releases/assets/7" || r.Header.Get("Accept") != "application/octet-stream" {
				t.Fatalf("unexpected asset lookup: %s; accept %q", r.URL, r.Header.Get("Accept"))
			}
			resp := githubResponse(r, 302, "")
			resp.Header.Set("Location", "https://release-assets.githubusercontent.com/github-production-release-asset/asset?token=signed")
			return resp, nil
		case 3:
			if r.URL.Hostname() != "release-assets.githubusercontent.com" || r.Header.Get("Authorization") != "" {
				t.Fatalf("unexpected download request: %#v", r)
			}
			return githubResponse(r, 200, "ZIP"), nil
		default:
			t.Fatalf("unexpected request: %s", r.URL)
			return nil, nil
		}
	}))
	got, source, err := client.Download(context.Background(), "Owner/Repo", "v1", 7)
	if err != nil || string(got) != "ZIP" || len(requests) != 3 || source != (GitHubSource{Repository: "owner/repo", Release: "v1", Asset: "theme.zip"}) {
		t.Fatalf("download = %q, source = %#v, requests = %#v, err = %v", got, source, requests, err)
	}
}

func TestGitHubDownloadRejectsUnmatchedOrInvalidAsset(t *testing.T) {
	for _, tc := range []struct{ name, release, kind, reason string }{
		{"wrong tag", `{"tag_name":"v2","assets":[{"id":7,"name":"theme.zip","size":3}]}`, "unavailable", "tag"},
		{"draft", `{"tag_name":"v1","draft":true,"assets":[{"id":7,"name":"theme.zip","size":3}]}`, "invalid_argument", "published"},
		{"absent", `{"tag_name":"v1","assets":[{"id":6,"name":"theme.zip","size":3}]}`, "invalid_argument", "asset"},
		{"source only", `{"tag_name":"v1","zipball_url":"https://api.github.com/repos/owner/repo/zipball/v1","assets":[]}`, "invalid_argument", "asset"},
		{"non zip", `{"tag_name":"v1","assets":[{"id":7,"name":"theme.tar.gz","size":3}]}`, "invalid_argument", "asset"},
		{"oversize", fmt.Sprintf(`{"tag_name":"v1","assets":[{"id":7,"name":"theme.zip","size":%d}]}`, MaxPackageBytes+1), "invalid_argument", "8 MiB"},
		{"duplicate", `{"tag_name":"v1","assets":[{"id":7,"name":"one.zip","size":3},{"id":7,"name":"two.zip","size":3}]}`, "unavailable", "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls != 1 {
					t.Fatalf("invalid asset triggered download: %s", r.URL)
				}
				return githubResponse(r, 200, tc.release), nil
			}))
			_, _, err := client.Download(context.Background(), "owner/repo", "v1", 7)
			requireGitHubError(t, err, tc.kind, tc.reason)
		})
	}
}

func TestGitHubRejectsUnsafeRedirects(t *testing.T) {
	for _, location := range []string{
		"http://api.github.com/assets/7", "https://127.0.0.1/asset", "https://169.254.169.254/asset",
		"https://release-assets.githubusercontent.com.evil.test/asset", "https://evil.test/asset",
		"https://user@github.com/asset", "https://github.com:8443/asset", "https://github.com/asset#fragment",
	} {
		t.Run(location, func(t *testing.T) {
			calls := 0
			client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls != 1 {
					t.Fatalf("unsafe redirect reached network: %s", r.URL)
				}
				resp := githubResponse(r, 302, "")
				resp.Header.Set("Location", location)
				return resp, nil
			}))
			_, err := client.Releases(context.Background(), "owner/repo")
			requireGitHubError(t, err, "unavailable", "destination")
		})
	}
}

func TestGitHubDownloadActualSizeLimit(t *testing.T) {
	for _, declared := range []int64{-1, MaxPackageBytes + 1} {
		t.Run(fmt.Sprint(declared), func(t *testing.T) {
			client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/tags/") {
					return githubResponse(r, 200, `{"tag_name":"v1","assets":[{"id":7,"name":"theme.zip","size":1}]}`), nil
				}
				resp := githubResponse(r, 200, strings.Repeat("X", MaxPackageBytes+1))
				resp.ContentLength = declared
				return resp, nil
			}))
			_, _, err := client.Download(context.Background(), "owner/repo", "v1", 7)
			requireGitHubError(t, err, "unavailable", "8 MiB")
		})
	}
}

func TestGitHubAPIErrorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		kind   string
		reason string
	}{
		{"rate limited", 403, "", "unavailable", "rate limit"},
		{"too many requests", 429, "", "unavailable", "rate limit"},
		{"server error", 503, "", "unavailable", "503"},
		{"not found", 404, "", "invalid_argument", "not found"},
		{"malformed", 200, "{", "unavailable", "JSON"},
		{"trailing document", 200, "[] []", "unavailable", "JSON"},
		{"large metadata", 200, strings.Repeat(" ", githubMaxMetadataBytes+1), "unavailable", "metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) { return githubResponse(r, tc.status, tc.body), nil }))
			_, err := client.Releases(context.Background(), "owner/repo")
			requireGitHubError(t, err, tc.kind, tc.reason)
		})
	}
	client := newGitHubClient(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded }))
	_, err := client.Releases(context.Background(), "owner/repo")
	requireGitHubError(t, err, "unavailable", "deadline")
}
