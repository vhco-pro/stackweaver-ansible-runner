// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// DefaultGalaxyServer is the public Galaxy, used when the mirror block names none.
const DefaultGalaxyServer = "https://galaxy.ansible.com/api/"

var galaxyName = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)

// GalaxyServer is a server the verifiers ask, in order: the mirror block's list, or the public one.
type GalaxyServer struct {
	URL   string
	Token string //nolint:gosec // G117: the server's API token
}

func (s GalaxyServer) headers() map[string]string {
	if s.Token == "" {
		return nil
	}
	return map[string]string{"Authorization": "Token " + s.Token}
}

func galaxyServers(servers []GalaxyServer) []GalaxyServer {
	if len(servers) == 0 {
		return []GalaxyServer{{URL: DefaultGalaxyServer}}
	}
	return servers
}

// CollectionVerifier promotes a Galaxy collection tarball only after it hashes to the
// artifact.sha256 the server's v3 collection version document publishes (D7, AC35).
type CollectionVerifier struct {
	Servers []GalaxyServer
	HTTP    *http.Client
}

// Verify implements Verifier. e.Name is the collection's namespace.name and e.Version its version.
func (v *CollectionVerifier) Verify(ctx context.Context, e runnercontract.CacheEntry, dir string) (*Artifact, error) {
	if !galaxyName.MatchString(e.Name) || e.Version == "" || strings.ContainsAny(e.Version, "/ ") {
		return nil, fmt.Errorf("%q %q is not a collection name and version", e.Name, e.Version)
	}
	ns, name, _ := strings.Cut(e.Name, ".")
	client := v.HTTP
	if client == nil {
		client = httpClient()
	}
	var lastErr error
	for _, s := range galaxyServers(v.Servers) {
		base, err := url.Parse(strings.TrimRight(s.URL, "/") + "/")
		if err != nil {
			lastErr = err
			continue
		}
		doc, err := base.Parse("v3/collections/" + ns + "/" + name + "/versions/" + url.PathEscape(e.Version) + "/")
		if err != nil {
			lastErr = err
			continue
		}
		raw, err := fetchBytes(ctx, client, doc.String(), s.headers())
		if err != nil {
			lastErr = err
			continue
		}
		var meta struct {
			DownloadURL string `json:"download_url"`
			Artifact    struct {
				Filename string `json:"filename"`
				SHA256   string `json:"sha256"`
			} `json:"artifact"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil || meta.DownloadURL == "" || !validDigest(strings.ToLower(meta.Artifact.SHA256)) {
			lastErr = fmt.Errorf("the server's document for %s %s carries no download_url and artifact.sha256", e.Name, e.Version)
			continue
		}
		dl, err := doc.Parse(meta.DownloadURL)
		if err != nil {
			lastErr = err
			continue
		}
		path := filepath.Join(dir, ns+"-"+name+"-"+e.Version+".tar.gz")
		got, size, err := fetchTo(ctx, client, dl.String(), path, s.headers())
		if err != nil {
			lastErr = err
			continue
		}
		if want := strings.ToLower(meta.Artifact.SHA256); got != want {
			return nil, fmt.Errorf("%w: %s %s hashes to %s, the server's artifact.sha256 is %s", ErrPoisoned, e.Name, e.Version, got, want)
		}
		return &Artifact{Path: path, SHA256: got, Size: size, SourceURL: dl.String(), Verification: Verified}, nil
	}
	return nil, fmt.Errorf("no Galaxy server served %s %s: %w", e.Name, e.Version, lastErr)
}

// RoleVerifier promotes a Galaxy role archive keyed by its tag's commit (D7): it reads the role's
// GitHub repository from the server, resolves the tag to its commit with git, fetches the archive of
// that commit, and records both, so a later tag move is never mistaken for the promoted entry.
type RoleVerifier struct {
	Servers []GalaxyServer
	HTTP    *http.Client
	// Git resolves a tag to its commit; nil means git ls-remote.
	Git GitRunner
	// ArchiveURL builds an archive URL of a commit; nil means GitHub's.
	ArchiveURL func(user, repo, commit string) string
}

// Verify implements Verifier. e.Name is the role's namespace.name and e.Version its tag.
func (v *RoleVerifier) Verify(ctx context.Context, e runnercontract.CacheEntry, dir string) (*Artifact, error) {
	if !galaxyName.MatchString(e.Name) || e.Version == "" || strings.ContainsAny(e.Version, " ") || strings.HasPrefix(e.Version, "-") {
		return nil, fmt.Errorf("%q %q is not a role name and tag", e.Name, e.Version)
	}
	ns, name, _ := strings.Cut(e.Name, ".")
	client := v.HTTP
	if client == nil {
		client = httpClient()
	}
	git := v.Git
	if git == nil {
		git = execGit{}
	}
	archive := v.ArchiveURL
	if archive == nil {
		archive = func(user, repo, commit string) string {
			return "https://github.com/" + url.PathEscape(user) + "/" + url.PathEscape(repo) + "/archive/" + commit + ".tar.gz"
		}
	}
	var lastErr error
	for _, s := range galaxyServers(v.Servers) {
		base, err := url.Parse(strings.TrimRight(s.URL, "/") + "/")
		if err != nil {
			lastErr = err
			continue
		}
		q, _ := base.Parse("v1/roles/?owner__username=" + url.QueryEscape(ns) + "&name=" + url.QueryEscape(name))
		raw, err := fetchBytes(ctx, client, q.String(), s.headers())
		if err != nil {
			lastErr = err
			continue
		}
		var found struct {
			Results []struct {
				GitHubUser string `json:"github_user"`
				GitHubRepo string `json:"github_repo"`
			} `json:"results"`
		}
		if err := json.Unmarshal(raw, &found); err != nil || len(found.Results) == 0 {
			lastErr = fmt.Errorf("the server knows no role %s", e.Name)
			continue
		}
		r := found.Results[0]
		repo := "https://github.com/" + r.GitHubUser + "/" + r.GitHubRepo
		commit, err := git.TagCommit(ctx, repo, e.Version)
		if err != nil {
			return nil, fmt.Errorf("resolving the tag %s of %s: %w", e.Version, repo, err)
		}
		path := filepath.Join(dir, ns+"."+name+"-"+e.Version+".tar.gz")
		src := archive(r.GitHubUser, r.GitHubRepo, commit)
		got, size, err := fetchTo(ctx, client, src, path, nil)
		if err != nil {
			return nil, err
		}
		return &Artifact{Path: path, SHA256: got, Size: size, SourceURL: src, Verification: Verified, Version: e.Version + "@" + commit}, nil
	}
	return nil, fmt.Errorf("no Galaxy server served the role %s: %w", e.Name, lastErr)
}
