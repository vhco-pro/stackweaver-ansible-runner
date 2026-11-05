// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

var (
	pipName      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	pipSeparator = regexp.MustCompile(`[-_.]+`)
	// pipAnchor is a link of a PEP 503 project page: its href and its text, the file name.
	pipAnchor = regexp.MustCompile(`(?is)<a\s[^>]*?href\s*=\s*["']([^"']+)["'][^>]*>([^<]+)</a>`)
)

// defaultPipIndex is where requirements resolve when the mirror block names no pip index.
const defaultPipIndex = "https://pypi.org/simple/"

// pipSimpleJSON is the PEP 691 media type of the simple API, which an index that serves it answers
// in place of the PEP 503 HTML page.
const pipSimpleJSON = "application/vnd.pypi.simple.v1+json"

// PipVerifier promotes a Python package only when the job's requirements pinned it by hash (D7). It
// reads the package index's simple API itself (PEP 503 HTML or PEP 691 JSON), takes the file of the
// pinned version whose published sha256 the requirements pin, downloads it and promotes it only when
// its bytes hash to that pin. It runs no pip: the launcher image carries none, and `pip download`
// prepares an sdist's metadata by running its build backend, which is user code the launcher never
// runs. A requirements file without hashes is installed into the job's own overlay and never
// promoted.
type PipVerifier struct {
	// IndexURL is the simple API's root (the mirror block's pip index); empty means PyPI.
	IndexURL string
	// TrustHost is a host whose index is used without a verified TLS certificate, as pip's
	// --trusted-host: the pins still decide what is promoted.
	TrustHost string
	HTTP      *http.Client
}

// pipFile is one file a project page lists.
type pipFile struct {
	name, url, sha256 string
}

// Verify implements Verifier. e.Name is the distribution's name, e.Version its exact version,
// e.Hashes its "sha256:<hex>" pins and e.SHA256, when the job reported it, the file it installed.
func (v *PipVerifier) Verify(ctx context.Context, e runnercontract.CacheEntry, dir string) (*Artifact, error) {
	var pins []string
	for _, h := range e.Hashes {
		if hex, ok := strings.CutPrefix(h, "sha256:"); ok && validDigest(strings.ToLower(hex)) {
			pins = append(pins, strings.ToLower(hex))
		}
	}
	if len(pins) == 0 {
		return nil, fmt.Errorf("%s %s is pinned by no sha256 hash; unhashed requirements are never promoted", e.Name, e.Version)
	}
	if !pipName.MatchString(e.Name) || !pipName.MatchString(e.Version) {
		return nil, fmt.Errorf("%q %q is not a package name and version", e.Name, e.Version)
	}
	if e.SHA256 != "" && !slices.Contains(pins, e.SHA256) {
		return nil, fmt.Errorf("the job reported %s %s as %s, which its requirements do not pin", e.Name, e.Version, e.SHA256)
	}
	index := v.IndexURL
	if index == "" {
		index = defaultPipIndex
	}
	base, err := url.Parse(strings.TrimRight(index, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("the pip index %q: %w", index, err)
	}
	project := normalizePipName(e.Name)
	page, err := base.Parse(project + "/")
	if err != nil {
		return nil, err
	}
	client := v.client(base)
	files, err := pipProjectFiles(ctx, client, page)
	if err != nil {
		return nil, err
	}
	pick, err := pickPipFile(files, project, e.Version, pins, e.SHA256)
	if err != nil {
		return nil, fmt.Errorf("%s %s on %s: %w", e.Name, e.Version, page, err)
	}
	src, err := page.Parse(pick.url)
	if err != nil {
		return nil, err
	}
	src.Fragment = ""
	path := filepath.Join(dir, filepath.Base(pick.name))
	got, size, err := fetchTo(ctx, client, src.String(), path, nil)
	if err != nil {
		return nil, err
	}
	if got != pick.sha256 {
		return nil, fmt.Errorf("%w: %s hashes to %s, the index publishes and the requirements pin %s", ErrPoisoned, pick.name, got, pick.sha256)
	}
	return &Artifact{Path: path, SHA256: got, Size: size, SourceURL: src.String(), Verification: Verified}, nil
}

// client is the verifier's HTTP client, without certificate verification for the trusted host.
func (v *PipVerifier) client(index *url.URL) *http.Client {
	c := v.HTTP
	if c == nil {
		c = httpClient()
	}
	if v.TrustHost == "" || (index.Host != v.TrustHost && index.Hostname() != v.TrustHost) {
		return c
	}
	t, ok := c.Transport.(*http.Transport)
	if !ok || t == nil {
		t = http.DefaultTransport.(*http.Transport) //nolint:forcetypeassert // the standard library's transport
	}
	t = t.Clone()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.TLSClientConfig != nil {
		cfg = t.TLSClientConfig.Clone()
	}
	cfg.InsecureSkipVerify = true //nolint:gosec // G402: pip's --trusted-host, set by the operator; the hash pins decide what is promoted
	t.TLSClientConfig = cfg
	trusted := *c
	trusted.Transport = t
	return &trusted
}

// normalizePipName is a project's name as the simple API addresses it (PEP 503).
func normalizePipName(name string) string {
	return strings.ToLower(pipSeparator.ReplaceAllString(name, "-"))
}

// pipProjectFiles reads a project page in either form of the simple API.
func pipProjectFiles(ctx context.Context, c *http.Client, page *url.URL) ([]pipFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", pipSimpleJSON+", text/html;q=0.1")
	resp, err := c.Do(req) //nolint:gosec // G107: the mirror block's or PyPI's index URL
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", page, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", page, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", page, err)
	}
	if media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); media == pipSimpleJSON {
		var doc struct {
			Files []struct {
				Filename string            `json:"filename"`
				URL      string            `json:"url"`
				Hashes   map[string]string `json:"hashes"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("reading %s: %w", page, err)
		}
		files := make([]pipFile, 0, len(doc.Files))
		for _, f := range doc.Files {
			files = append(files, pipFile{name: f.Filename, url: f.URL, sha256: strings.ToLower(f.Hashes["sha256"])})
		}
		return files, nil
	}
	var files []pipFile
	for _, m := range pipAnchor.FindAllStringSubmatch(string(body), -1) {
		href := html.UnescapeString(m[1])
		f := pipFile{name: strings.TrimSpace(html.UnescapeString(m[2])), url: href}
		if _, frag, ok := strings.Cut(href, "#"); ok {
			if d, ok := strings.CutPrefix(frag, "sha256="); ok {
				f.sha256 = strings.ToLower(d)
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// pickPipFile chooses the file of project's version the pins prove: the one the job reported by its
// digest when it did, else the only pinned file, else the pinned pure-Python wheel. A file the index
// lists without a sha256 is never chosen, and platform builds are never guessed between.
func pickPipFile(files []pipFile, project, version string, pins []string, reported string) (pipFile, error) {
	var pinned []pipFile
	listed := false
	for _, f := range files {
		if !pipFileOf(f.name, project, version) {
			continue
		}
		listed = true
		if validDigest(f.sha256) && slices.Contains(pins, f.sha256) {
			pinned = append(pinned, f)
		}
	}
	switch {
	case !listed:
		return pipFile{}, errors.New("the index lists no file of this version")
	case len(pinned) == 0:
		return pipFile{}, errors.New("the index publishes no sha256 the requirements pin for any file of this version")
	case reported != "":
		for _, f := range pinned {
			if f.sha256 == reported {
				return f, nil
			}
		}
		return pipFile{}, fmt.Errorf("the index lists no file hashing to %s, the one the job installed", reported)
	case len(pinned) == 1:
		return pinned[0], nil
	}
	var pure []pipFile
	for _, f := range pinned {
		if strings.HasSuffix(f.name, "-none-any.whl") {
			pure = append(pure, f)
		}
	}
	if len(pure) == 1 {
		return pure[0], nil
	}
	return pipFile{}, fmt.Errorf("the requirements pin %d files of this version and the job reported none of them", len(pinned))
}

// pipFileOf reports whether a file name is a wheel or sdist of project's version.
func pipFileOf(name, project, version string) bool {
	if stem, ok := strings.CutSuffix(name, ".whl"); ok {
		parts := strings.Split(stem, "-")
		return len(parts) >= 5 && normalizePipName(parts[0]) == project && parts[1] == version
	}
	for _, ext := range []string{".tar.gz", ".zip", ".tar.bz2", ".tgz"} {
		if stem, ok := strings.CutSuffix(name, ext); ok {
			n, ok := strings.CutSuffix(stem, "-"+version)
			return ok && normalizePipName(n) == project
		}
	}
	return false
}
