// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cachedesc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"reflect"
	"testing"
)

const lockFile = `# This file is maintained automatically by "tofu init".
provider "registry.opentofu.org/hashicorp/null" {
  version     = "3.2.4"
  constraints = "~> 3.2"
  hashes = [
    "h1:i+WKhUHL2REY5EGmiHjfUljJB8UKZ9QdhdM5uTeUhC4=",
    "zh:3d106c7e32a929e2843f732625a582e562ff09120021e510a51a6f5d01175b8d",
    "zh:ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
  ]
}

provider "registry.opentofu.org/hashicorp/random" {
  version = "3.6.3"
}
`

func TestParseLockFile(t *testing.T) {
	got, err := ParseLockFile([]byte(lockFile))
	if err != nil {
		t.Fatal(err)
	}
	want := []LockedProvider{
		{Source: "registry.opentofu.org/hashicorp/null", Version: "3.2.4", Hashes: []string{
			"h1:i+WKhUHL2REY5EGmiHjfUljJB8UKZ9QdhdM5uTeUhC4=",
			"zh:3d106c7e32a929e2843f732625a582e562ff09120021e510a51a6f5d01175b8d",
			"zh:ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		}},
		{Source: "registry.opentofu.org/hashicorp/random", Version: "3.6.3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseLockFile() = %+v, want %+v", got, want)
	}
	if zh := got[0].ZipHashes(); !reflect.DeepEqual(zh, []string{"3d106c7e32a929e2843f732625a582e562ff09120021e510a51a6f5d01175b8d", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}) {
		t.Errorf("ZipHashes() = %v", zh)
	}
	if _, err := ParseLockFile([]byte(`provider "x" {`)); err == nil {
		t.Error("a malformed lock file parsed")
	}
}

func TestParseRequirements(t *testing.T) {
	cases := map[string]struct {
		src         string
		collections []Requirement
		roles       []Requirement
	}{
		"both sections, mixed forms": {
			src: `
collections:
  - ansible.posix
  - name: community.general
    version: "9.4.0"
  - name: https://github.com/example/collection.git
    type: git
    version: 0123456789abcdef0123456789abcdef01234567
  - name: amazon.aws
    version: ">=8.0.0"
roles:
  - geerlingguy.docker
  - name: geerlingguy.pip
    version: 3.0.3
  - src: https://github.com/example/role.git
    scm: git
    version: main
`,
			collections: []Requirement{
				{Name: "ansible.posix"},
				{Name: "community.general", Version: "9.4.0"},
				{Name: "https://github.com/example/collection.git", Type: "git", Version: "0123456789abcdef0123456789abcdef01234567"},
				{Name: "amazon.aws", Version: ">=8.0.0"},
			},
			roles: []Requirement{
				{Name: "geerlingguy.docker"},
				{Name: "geerlingguy.pip", Version: "3.0.3"},
				{Name: "https://github.com/example/role.git", Type: "git", Version: "main"},
			},
		},
		"the legacy roles-only list": {
			src:   "- src: geerlingguy.java\n  version: 2.3.1\n",
			roles: []Requirement{{Name: "geerlingguy.java", Version: "2.3.1"}},
		},
		"a git+ name": {
			src:         "collections:\n  - name: git+https://github.com/example/c.git,v1.0.0\n",
			collections: []Requirement{{Name: "git+https://github.com/example/c.git,v1.0.0", Type: "git"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRequirements([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Collections, tc.collections) || !reflect.DeepEqual(got.Roles, tc.roles) {
				t.Errorf("got collections %+v roles %+v", got.Collections, got.Roles)
			}
		})
	}
	if r := (Requirement{Version: "9.4.0"}); !r.Pinned() {
		t.Error("9.4.0 is a pin")
	}
	for _, v := range []string{"", "*", ">=8.0.0", "==1.0.0,!=1.0.1", "main"} {
		if (Requirement{Version: v}).Pinned() && v != "main" {
			t.Errorf("%q is not a pin", v)
		}
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestFindInTarGz(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"./main.tf":                       "resource {}",
		"./envs/prod/.terraform.lock.hcl": "lock",
		"roles/requirements.yml":          "roles: []",
	})
	got, name, err := FindInTarGz(bytes.NewReader(archive), []string{"envs/prod/.terraform.lock.hcl"}, 1<<20)
	if err != nil || string(got) != "lock" || name != "envs/prod/.terraform.lock.hcl" {
		t.Errorf("FindInTarGz() = %q %q %v", got, name, err)
	}
	got, name, err = FindInTarGz(bytes.NewReader(archive), []string{"requirements.yml", "collections/requirements.yml", "roles/requirements.yml"}, 1<<20)
	if err != nil || string(got) != "roles: []" || name != "roles/requirements.yml" {
		t.Errorf("FindInTarGz() = %q %q %v", got, name, err)
	}
	if got, _, err := FindInTarGz(bytes.NewReader(archive), []string{"absent"}, 1<<20); err != nil || got != nil {
		t.Errorf("an absent file: %q %v", got, err)
	}
	if _, _, err := FindInTarGz(bytes.NewReader(archive), []string{"./main.tf"}, 3); err == nil {
		t.Error("a file over the cap was read")
	}
}

func TestGitSource(t *testing.T) {
	for _, tc := range []struct{ name, repo, subdir string }{
		{"https://github.com/example/c.git", "https://github.com/example/c.git", ""},
		{"git+https://github.com/example/c.git", "https://github.com/example/c.git", ""},
		{"git+https://github.com/example/c.git#/collections/c/", "https://github.com/example/c.git", "collections/c"},
		{"https://github.com/example/c.git#collections/c", "https://github.com/example/c.git", "collections/c"},
	} {
		if repo, subdir := GitSource(tc.name); repo != tc.repo || subdir != tc.subdir {
			t.Errorf("GitSource(%q) = %q, %q, want %q, %q", tc.name, repo, subdir, tc.repo, tc.subdir)
		}
	}
}
