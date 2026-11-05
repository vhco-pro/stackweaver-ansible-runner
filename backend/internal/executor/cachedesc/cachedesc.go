// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package cachedesc reads what a job's configuration pins (#933, D7): the providers and their
// hashes of an OpenTofu dependency lock file and the collections and roles of an Ansible
// requirements file. The API resolves a job's cache descriptor from them, and the executor reports
// what it installed beyond the descriptor with the same readings.
package cachedesc

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
)

// LockFileName is the dependency lock file OpenTofu writes beside a configuration.
const LockFileName = ".terraform.lock.hcl"

// RequirementsFiles are where a playbook repository carries its Galaxy requirements, in the order
// ansible-galaxy's callers look.
var RequirementsFiles = []string{"requirements.yml", "collections/requirements.yml", "roles/requirements.yml"}

// LockedProvider is one provider block of a lock file.
type LockedProvider struct {
	Source  string
	Version string
	Hashes  []string
}

// ZipHashes are the provider's zh: hashes, the sha256 of its package zips, lowercase.
func (p LockedProvider) ZipHashes() []string {
	var out []string
	for _, h := range p.Hashes {
		if hex, ok := strings.CutPrefix(h, "zh:"); ok && len(hex) == 64 {
			out = append(out, strings.ToLower(hex))
		}
	}
	return out
}

// ParseLockFile reads the provider blocks of a dependency lock file.
func ParseLockFile(data []byte) ([]LockedProvider, error) {
	f, diags := hclparse.NewParser().ParseHCL(data, LockFileName)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parsing the lock file: %s", diags.Error())
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, errors.New("parsing the lock file: unexpected body")
	}
	var out []LockedProvider
	for _, b := range body.Blocks {
		if b.Type != "provider" || len(b.Labels) != 1 {
			continue
		}
		p := LockedProvider{Source: b.Labels[0]}
		if a, ok := b.Body.Attributes["version"]; ok {
			if v, d := a.Expr.Value(nil); !d.HasErrors() && v.Type() == cty.String {
				p.Version = v.AsString()
			}
		}
		if a, ok := b.Body.Attributes["hashes"]; ok {
			v, d := a.Expr.Value(nil)
			if d.HasErrors() || (!v.Type().IsTupleType() && !v.Type().IsListType()) {
				return nil, fmt.Errorf("parsing the lock file: the hashes of %s are not a list", p.Source)
			}
			for it := v.ElementIterator(); it.Next(); {
				_, h := it.Element()
				if h.Type() == cty.String {
					p.Hashes = append(p.Hashes, h.AsString())
				}
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Requirement is one collection or role of a requirements file.
type Requirement struct {
	// Name is the namespace.name, or the source URL of a git or URL requirement.
	Name    string
	Version string
	// Type is "git" for a git source, "url" or "file" for those, empty for Galaxy.
	Type string
}

var exactVersion = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)*([-+][0-9A-Za-z.-]+)?$`)

// Pinned reports whether the version names exactly one release (no range, no wildcard). A git
// requirement's branch or tag is not judged here.
func (r Requirement) Pinned() bool { return exactVersion.MatchString(r.Version) }

// Galaxy reports whether the requirement comes from a Galaxy server.
func (r Requirement) Galaxy() bool { return r.Type == "" || r.Type == "galaxy" }

// GitSource splits a git requirement's name into the repository it clones and the subdirectory of
// the repository it installs from: ansible-galaxy reads "git+<url>#/<path>/" as the collection at
// <path> of <url>. A git source is cached by its repository and commit, whatever subdirectory a
// requirement installs from (#933, D7).
func GitSource(name string) (repo, subdir string) {
	repo, subdir, _ = strings.Cut(strings.TrimPrefix(name, "git+"), "#")
	return repo, strings.Trim(subdir, "/")
}

// Requirements are a requirements file's collections and roles.
type Requirements struct {
	Collections []Requirement
	Roles       []Requirement
}

// ParseRequirements reads a requirements file in either form ansible-galaxy accepts: a mapping
// with collections and roles, or the older bare list of roles.
func ParseRequirements(data []byte) (Requirements, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return Requirements{}, fmt.Errorf("parsing the requirements: %w", err)
	}
	var out Requirements
	if len(node.Content) == 0 {
		return out, nil
	}
	root := node.Content[0]
	switch root.Kind {
	case yaml.SequenceNode:
		out.Roles = requirementList(root)
	case yaml.MappingNode:
		for i := 0; i+1 < len(root.Content); i += 2 {
			switch root.Content[i].Value {
			case "collections":
				out.Collections = requirementList(root.Content[i+1])
			case "roles":
				out.Roles = requirementList(root.Content[i+1])
			}
		}
	case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode:
		return out, errors.New("parsing the requirements: neither a mapping nor a list")
	}
	return out, nil
}

func requirementList(n *yaml.Node) []Requirement {
	if n.Kind != yaml.SequenceNode {
		return nil
	}
	var out []Requirement
	for _, item := range n.Content {
		var r Requirement
		switch item.Kind {
		case yaml.ScalarNode:
			r.Name = item.Value
		case yaml.MappingNode:
			fields := map[string]string{}
			for i := 0; i+1 < len(item.Content); i += 2 {
				fields[item.Content[i].Value] = item.Content[i+1].Value
			}
			r.Name = fields["name"]
			if src := fields["src"]; src != "" && (r.Name == "" || strings.Contains(src, "://")) {
				r.Name = src
			}
			r.Version = fields["version"]
			r.Type = firstNonEmpty(fields["type"], fields["scm"], fields["source_type"])
		case yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode:
			continue
		}
		if r.Name == "" {
			continue
		}
		if strings.HasPrefix(r.Name, "git+") {
			r.Type = "git"
		}
		if r.Type == "galaxy" {
			r.Type = ""
		}
		out = append(out, r)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// FindInTarGz reads the first of names (relative paths, in priority order) that a gzipped tar
// carries, up to maxBytes; nil and no error when it carries none of them.
func FindInTarGz(r io.Reader, names []string, maxBytes int64) ([]byte, string, error) {
	want := map[string]int{}
	for i, n := range names {
		want[clean(n)] = i
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, "", fmt.Errorf("reading the archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	found := map[int][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("reading the archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		i, ok := want[clean(h.Name)]
		if !ok {
			continue
		}
		if h.Size > maxBytes {
			return nil, "", fmt.Errorf("%s is larger than %d bytes", h.Name, maxBytes)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxBytes))
		if err != nil {
			return nil, "", fmt.Errorf("reading %s: %w", h.Name, err)
		}
		found[i] = b
	}
	for i, n := range names {
		if b, ok := found[i]; ok {
			return b, clean(n), nil
		}
	}
	return nil, "", nil
}

func clean(p string) string { return strings.TrimPrefix(path.Clean("/"+p), "/") }
