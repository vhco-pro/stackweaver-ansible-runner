// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// pipIndex is a package index serving the simple API (PEP 503 HTML or PEP 691 JSON) for one
// project, with each file's published sha256, and recording every path it served.
type pipIndex struct {
	project string // the normalized project name
	files   map[string][]byte
	// published overrides a file's published digest (an index whose file and hash disagree).
	published map[string]string
	json      bool
	mu        sync.Mutex
	paths     []string
}

func (x *pipIndex) digestOf(name string) string {
	if d, ok := x.published[name]; ok {
		return d
	}
	return digest(x.files[name])
}

func (x *pipIndex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	x.mu.Lock()
	x.paths = append(x.paths, r.URL.Path)
	x.mu.Unlock()
	if r.URL.Path == "/simple/"+x.project+"/" {
		if x.json && strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			var entries []string
			for name := range x.files {
				entries = append(entries, fmt.Sprintf(`{"filename":%q,"url":"/packages/%s","hashes":{"sha256":%q}}`, name, name, x.digestOf(name)))
			}
			_, _ = fmt.Fprintf(w, `{"meta":{"api-version":"1.1"},"name":%q,"files":[%s]}`, x.project, strings.Join(entries, ","))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, "<!DOCTYPE html><html><body>\n")
		for name := range x.files {
			_, _ = fmt.Fprintf(w, "<a href=\"../../packages/%s#sha256=%s\" data-requires-python=\"&gt;=3.8\">%s</a><br/>\n", name, x.digestOf(name), name)
		}
		_, _ = fmt.Fprint(w, "</body></html>\n")
		return
	}
	if name, ok := strings.CutPrefix(r.URL.Path, "/packages/"); ok {
		if body, found := x.files[name]; found {
			_, _ = w.Write(body)
			return
		}
	}
	http.NotFound(w, r)
}

func (x *pipIndex) served() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string(nil), x.paths...)
}

func pins(bodies ...[]byte) []string {
	var out []string
	for _, b := range bodies {
		out = append(out, "sha256:"+digest(b))
	}
	return out
}

// TestPipVerifier_ReadsTheIndexNotPip: the launcher image has no pip, so the populator verifies a
// hash-pinned requirement from the index's simple API itself (D7): it downloads the file the index
// lists for the pinned version whose published sha256 the requirements pin, and promotes it only
// when its bytes hash to that pin. Both forms of the simple API are read.
func TestPipVerifier_ReadsTheIndexNotPip(t *testing.T) {
	wheel := []byte("requests 2.32.3 wheel")
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "PEP 503 HTML", true: "PEP 691 JSON"}[asJSON], func(t *testing.T) {
			idx := &pipIndex{project: "requests", json: asJSON, files: map[string][]byte{
				"requests-2.32.3-py3-none-any.whl": wheel,
				"requests-2.32.2-py3-none-any.whl": []byte("an older wheel"),
			}}
			srv := httptest.NewServer(idx)
			defer srv.Close()
			v := &PipVerifier{IndexURL: srv.URL + "/simple"}
			e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindPip, Name: "Requests", Version: "2.32.3", Hashes: pins(wheel)}
			a, err := v.Verify(t.Context(), e, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if a.SHA256 != digest(wheel) || a.Size != int64(len(wheel)) || a.Verification != Verified {
				t.Errorf("artifact = %+v", a)
			}
			got, _ := os.ReadFile(a.Path) //nolint:gosec // G304: the verifier's scratch file
			if string(got) != string(wheel) {
				t.Errorf("the artifact holds %q", got)
			}
			if !strings.HasSuffix(a.SourceURL, "/packages/requests-2.32.3-py3-none-any.whl") {
				t.Errorf("source = %s", a.SourceURL)
			}
			if p := idx.served(); len(p) != 2 || p[0] != "/simple/requests/" {
				t.Errorf("served %v, want the project page then the file", p)
			}
		})
	}
}

// TestPipVerifier_RefusesWhatThePinsDoNotProve covers the refusals: a file whose bytes differ from
// the hash the index publishes for it is an attempted poisoning; a version the index has no pinned
// file for, and an unhashed requirement, are not promoted and nothing is fetched for the latter.
func TestPipVerifier_RefusesWhatThePinsDoNotProve(t *testing.T) {
	wheel := []byte("iniconfig wheel")
	idx := &pipIndex{project: "iniconfig", files: map[string][]byte{"iniconfig-2.1.0-py3-none-any.whl": wheel}}
	srv := httptest.NewServer(idx)
	defer srv.Close()
	v := &PipVerifier{IndexURL: srv.URL + "/simple/"}
	e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindPip, Name: "iniconfig", Version: "2.1.0"}

	// The index publishes the pinned digest for the file but serves other bytes.
	tampered := []byte("the wheel the job pinned")
	idx.published = map[string]string{"iniconfig-2.1.0-py3-none-any.whl": digest(tampered)}
	e.Hashes = pins(tampered)
	if _, err := v.Verify(t.Context(), e, t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a file that does not hash to its published, pinned digest: %v, want ErrPoisoned", err)
	}
	idx.published = nil

	// No file of the version carries a pinned digest.
	e.Hashes = pins([]byte("some other wheel"))
	if _, err := v.Verify(t.Context(), e, t.TempDir()); err == nil || errors.Is(err, ErrPoisoned) {
		t.Errorf("no pinned file on the index: %v, want a refusal that is not a poisoning", err)
	}
	e.Hashes = pins(wheel)
	e.Version = "9.9.9"
	if _, err := v.Verify(t.Context(), e, t.TempDir()); err == nil || errors.Is(err, ErrPoisoned) {
		t.Errorf("a version the index does not have: %v, want a refusal that is not a poisoning", err)
	}

	before := len(idx.served())
	e.Version, e.Hashes = "2.1.0", nil
	if _, err := v.Verify(t.Context(), e, t.TempDir()); err == nil {
		t.Error("an unhashed requirement was promoted")
	}
	if len(idx.served()) != before {
		t.Error("the index was read for an unhashed requirement")
	}
	e.Name, e.Hashes = "../etc", pins(wheel)
	if _, err := v.Verify(t.Context(), e, t.TempDir()); err == nil {
		t.Error("a name that is not a package name was looked up")
	}
}

// TestPipVerifier_PicksTheFileTheJobInstalled: a requirement may pin several files of a version
// (a wheel per platform and the sdist). The populator takes the one the job reported by its digest,
// or, when the job reported none, the one pinned file or else the pinned pure-Python wheel; it
// never guesses between platform builds.
func TestPipVerifier_PicksTheFileTheJobInstalled(t *testing.T) {
	linux := []byte("zope.interface manylinux wheel")
	mac := []byte("zope.interface macos wheel")
	sdist := []byte("zope.interface sdist")
	idx := &pipIndex{project: "zope-interface", files: map[string][]byte{
		"zope_interface-7.2-cp312-cp312-manylinux_2_17_x86_64.whl": linux,
		"zope_interface-7.2-cp312-cp312-macosx_11_0_arm64.whl":     mac,
		"zope_interface-7.2.tar.gz":                                sdist,
	}}
	srv := httptest.NewServer(idx)
	defer srv.Close()
	v := &PipVerifier{IndexURL: srv.URL + "/simple"}
	e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindPip, Name: "zope.interface", Version: "7.2", Hashes: pins(linux, mac, sdist)}

	e.SHA256 = digest(linux)
	a, err := v.Verify(t.Context(), e, t.TempDir())
	if err != nil || a.SHA256 != digest(linux) {
		t.Fatalf("the reported file: %+v, %v", a, err)
	}
	e.SHA256 = digest([]byte("a file the requirements do not pin"))
	if _, err := v.Verify(t.Context(), e, t.TempDir()); err == nil {
		t.Error("a reported file the requirements do not pin was promoted")
	}
	e.SHA256 = ""
	if _, err := v.Verify(t.Context(), e, t.TempDir()); err == nil {
		t.Error("the populator guessed between platform builds")
	}
	e.Hashes = pins(sdist)
	if a, err := v.Verify(t.Context(), e, t.TempDir()); err != nil || a.SHA256 != digest(sdist) {
		t.Errorf("the one pinned file: %+v, %v", a, err)
	}
}
