// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/executor/cachedesc"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/services/ansible"
	"gopkg.in/yaml.v3"
)

var (
	galaxyFQCN = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)
	fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// seeded is what a job took from its organization's cache.
type seeded struct {
	collections map[string]bool // "ns.name version"
	roles       map[string]bool // "ns.name"
	git         map[string]bool // "url commit"
	// rewritten is a copy of the requirements that installs cached git sources from their
	// verified archives; empty when there are none.
	rewritten string
}

func (s seeded) summary() string {
	var parts []string
	plural := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	if n := len(s.collections); n > 0 {
		parts = append(parts, plural(n, "collection"))
	}
	if n := len(s.roles); n > 0 {
		parts = append(parts, plural(n, "role"))
	}
	if n := len(s.git); n > 0 {
		parts = append(parts, plural(n, "git source"))
	}
	return strings.Join(parts, ", ")
}

func readRequirements(path string) (cachedesc.Requirements, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the job's own playbook repository
	if err != nil {
		return cachedesc.Requirements{}, err
	}
	return cachedesc.ParseRequirements(data)
}

// seedFromCache installs what the descriptor names from the verified cache into the staging
// directories before the requirements run: collections from their tarballs, roles from their
// archives, and git sources through a rewritten copy of the requirements that points each source
// whose commit the cache holds at its verified archive. An entry that cannot be read or installed
// is simply left to its source.
func (in GalaxyInstall) seedFromCache(ctx context.Context, requirementsPath string, reqs cachedesc.Requirements, commits map[string]string, stageCollections, stageRoles string) seeded {
	out := seeded{collections: map[string]bool{}, roles: map[string]bool{}, git: map[string]bool{}}
	if in.Cache == nil || in.Cache.Reader == nil || len(in.Cache.Entries) == 0 {
		return out
	}
	dir := filepath.Join(in.JobDir, "galaxy", "cached")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return out
	}
	fetch := func(e runnercontract.CacheEntryRef, name string) string {
		dst := filepath.Join(dir, name)
		src, err := in.Cache.Reader.Fetch(ctx, e, dst)
		if err != nil || src == executor.SourceNone {
			if err != nil {
				logger.Warnf("The cached %s %s could not be used: %v", e.Kind, e.Name, err)
			}
			return ""
		}
		return dst
	}

	var tarballs []string
	tarballFor := map[string]string{}
	var roleLines []string
	// The repositories at the commits the job's git sources install now; an entry at another
	// commit of the same repository is not fetched.
	wanted := map[string]bool{}
	for _, r := range reqs.Collections {
		if commit := commits[gitKey(r)]; commit != "" {
			repo, _ := cachedesc.GitSource(r.Name)
			wanted[repo+" "+commit] = true
		}
	}
	archives := map[string]string{}
	for _, e := range in.Cache.Entries {
		switch e.Kind {
		case runnercontract.CacheKindCollection:
			if !galaxyFQCN.MatchString(e.Name) || !validSegment(e.Version) {
				continue
			}
			if p := fetch(e, strings.ReplaceAll(e.Name, ".", "-")+"-"+e.Version+".tar.gz"); p != "" {
				tarballs = append(tarballs, p)
				tarballFor[p] = e.Name + " " + e.Version
			}
		case runnercontract.CacheKindRole:
			if !galaxyFQCN.MatchString(e.Name) {
				continue
			}
			if p := fetch(e, "role-"+e.Name+".tar.gz"); p != "" {
				roleLines = append(roleLines, fmt.Sprintf("- src: %q\n  name: %q\n", "file://"+p, e.Name))
				out.roles[e.Name] = true
			}
		case runnercontract.CacheKindGit:
			key := e.Name + " " + e.Version
			if !fullCommit.MatchString(e.Version) || !wanted[key] || archives[key] != "" {
				continue
			}
			if p := fetch(e, "git-"+e.SHA256+".tar.gz"); p != "" {
				target := filepath.Join(dir, "git-"+e.SHA256)
				if err := extractTarGz(p, target); err == nil {
					archives[key] = filepath.Join(target, "source")
				}
			}
		case runnercontract.CacheKindTofu, runnercontract.CacheKindProvider, runnercontract.CacheKindPip:
		}
	}

	if len(tarballs) > 0 {
		args := append([]string{"collection", "install", "--offline", "--no-deps", "-p", stageCollections}, tarballs...)
		if output, err := in.galaxyCommand(ctx, args...).CombinedOutput(); err != nil {
			// A partial seed is discarded: the requirements install the lot from their sources.
			logger.Warnf("Installing the cached collections failed, installing from the sources: %v: %s", err, output)
			_ = os.RemoveAll(stageCollections)
			_ = os.MkdirAll(stageCollections, 0o755) //nolint:gosec // workspace directories need 0o755 for compatibility
		} else {
			for _, p := range tarballs {
				out.collections[tarballFor[p]] = true
			}
		}
	}
	if len(roleLines) > 0 {
		roles := filepath.Join(dir, "cached-roles.yml")
		if err := os.WriteFile(roles, []byte(strings.Join(roleLines, "")), 0o600); err == nil {
			if output, err := in.galaxyCommand(ctx, "role", "install", "-r", roles, "-p", stageRoles).CombinedOutput(); err != nil {
				logger.Warnf("Installing the cached roles failed, installing from the sources: %v: %s", err, output)
				out.roles = map[string]bool{}
			}
		}
	}
	gitDirs := map[string]string{}
	for _, r := range reqs.Collections {
		key := gitKey(r)
		repo, subdir := cachedesc.GitSource(r.Name)
		source := archives[repo+" "+commits[key]]
		if source == "" {
			continue
		}
		// The subdirectory the requirement installs from, which may not leave the archive.
		dir := filepath.Join(source, filepath.FromSlash(subdir))
		if rel, err := filepath.Rel(source, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		gitDirs[key] = dir
	}
	if len(gitDirs) > 0 {
		if path, used := rewriteGitRequirements(requirementsPath, gitDirs); path != "" {
			out.rewritten = path
			for k := range used {
				out.git[k] = true
			}
		}
	}
	return out
}

// rewriteGitRequirements writes a copy of the requirements beside them in which every git source
// whose commit the cache holds installs from its verified archive instead (type: dir).
func rewriteGitRequirements(path string, dirs map[string]string) (string, map[string]bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the job's own playbook repository
	if err != nil {
		return "", nil
	}
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", nil
	}
	used := map[string]bool{}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "collections" || root.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range root.Content[i+1].Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			fields := map[string]*yaml.Node{}
			for j := 0; j+1 < len(item.Content); j += 2 {
				fields[item.Content[j].Value] = item.Content[j+1]
			}
			name, version, typ := fields["name"], fields["version"], fields["type"]
			if name == nil {
				continue
			}
			key := strings.TrimPrefix(name.Value, "git+") + " "
			if version != nil {
				key += version.Value
			}
			dir, ok := dirs[key]
			if !ok || (typ != nil && typ.Value != "git") && !strings.HasPrefix(name.Value, "git+") {
				continue
			}
			name.Value = dir
			if typ != nil {
				typ.Value = "dir"
			} else {
				item.Content = append(item.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "type"}, &yaml.Node{Kind: yaml.ScalarNode, Value: "dir"})
			}
			// A directory install takes no version.
			for j := 0; j+1 < len(item.Content); j += 2 {
				if item.Content[j].Value == "version" {
					item.Content = append(item.Content[:j], item.Content[j+2:]...)
					break
				}
			}
			used[key] = true
		}
	}
	if len(used) == 0 {
		return "", nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", nil
	}
	rewritten := filepath.Join(filepath.Dir(path), ".stackweaver-requirements.yml")
	if err := os.WriteFile(rewritten, out, 0o600); err != nil {
		return "", nil
	}
	return rewritten, used
}

// installedBeyond reports what the requirements installed that the cache did not supply: the
// collections a Galaxy server served and Galaxy roles by name and version, and git sources by
// repository at the commit they resolved to. The populator fetches each from its source again and
// verifies it; nothing here is trusted.
func installedBeyond(stageCollections, stageRoles string, reqs cachedesc.Requirements, commits map[string]string, from seeded) []runnercontract.CacheEntry {
	var out []runnercontract.CacheEntry
	nonGalaxy := map[string]bool{}
	for _, r := range append(append([]cachedesc.Requirement{}, reqs.Collections...), reqs.Roles...) {
		if !r.Galaxy() {
			nonGalaxy[r.Name] = true
		}
	}
	manifests, _ := filepath.Glob(filepath.Join(stageCollections, "ansible_collections", "*", "*", "MANIFEST.json"))
	for _, m := range manifests {
		var doc struct {
			CollectionInfo struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
				Version   string `json:"version"`
			} `json:"collection_info"`
		}
		data, err := os.ReadFile(m) //nolint:gosec // G304: the job's staging directory
		if err != nil || json.Unmarshal(data, &doc) != nil {
			continue
		}
		fqcn := doc.CollectionInfo.Namespace + "." + doc.CollectionInfo.Name
		if !galaxyFQCN.MatchString(fqcn) || doc.CollectionInfo.Version == "" || from.collections[fqcn+" "+doc.CollectionInfo.Version] {
			continue
		}
		// ansible-galaxy writes a collection's source metadata only when it came from a Galaxy
		// server; one built from a git source or a directory has none, and is no Galaxy entry.
		if _, err := os.Stat(filepath.Join(stageCollections, "ansible_collections", fqcn+"-"+doc.CollectionInfo.Version+".info")); err != nil {
			continue
		}
		out = append(out, runnercontract.CacheEntry{Kind: runnercontract.CacheKindCollection, Name: fqcn, Version: doc.CollectionInfo.Version})
	}
	infos, _ := filepath.Glob(filepath.Join(stageRoles, "*", "meta", ".galaxy_install_info"))
	for _, info := range infos {
		role := filepath.Base(filepath.Dir(filepath.Dir(info)))
		if !galaxyFQCN.MatchString(role) || from.roles[role] || nonGalaxy[role] {
			continue
		}
		data, err := os.ReadFile(info) //nolint:gosec // G304: the job's staging directory
		if err != nil {
			continue
		}
		var meta struct {
			Version string `yaml:"version"`
		}
		if yaml.Unmarshal(data, &meta) != nil || meta.Version == "" || !validSegment(meta.Version) {
			continue
		}
		out = append(out, runnercontract.CacheEntry{Kind: runnercontract.CacheKindRole, Name: role, Version: meta.Version})
	}
	reported := map[string]bool{}
	for _, r := range reqs.Collections {
		key := gitKey(r)
		commit := commits[key]
		repo, _ := cachedesc.GitSource(r.Name)
		if r.Type != "git" || commit == "" || from.git[key] || reported[repo+" "+commit] {
			continue
		}
		reported[repo+" "+commit] = true
		out = append(out, runnercontract.CacheEntry{Kind: runnercontract.CacheKindGit, Name: repo, Version: commit})
	}
	return out
}

// gitKey names a git requirement as the rewritten requirements do: its name without git+ and its
// version, empty for the repository's default branch.
func gitKey(r cachedesc.Requirement) string {
	return strings.TrimPrefix(r.Name, "git+") + " " + r.Version
}

// gitCommits resolves each git source of reqs to the commit it installs now, keyed by gitKey (#933,
// D7: a git source is cached by its resolved commit). A full commit is its own; a branch, a tag or
// no version (the default branch) is resolved with ResolveRef. One that does not resolve is left
// out: it installs from its source as written and is not cached. Nothing is resolved for a job
// without a cache.
func (in GalaxyInstall) gitCommits(ctx context.Context, reqs cachedesc.Requirements) map[string]string {
	out := map[string]string{}
	if in.Cache == nil {
		return out
	}
	resolve := in.ResolveRef
	if resolve == nil {
		resolve = in.lsRemote
	}
	for _, r := range reqs.Collections {
		key := gitKey(r)
		if _, done := out[key]; done || r.Type != "git" {
			continue
		}
		if fullCommit.MatchString(r.Version) {
			out[key] = r.Version
			continue
		}
		repo, _ := cachedesc.GitSource(r.Name)
		commit, err := resolve(ctx, repo, r.Version)
		if err != nil || !fullCommit.MatchString(commit) {
			logger.Warnf("Resolving %s %q to a commit failed, it is installed from its source: %v", repo, r.Version, err)
			continue
		}
		out[key] = commit
	}
	return out
}

// gitResolveTimeout bounds resolving one git ref.
const gitResolveTimeout = 30 * time.Second

// lsRemote resolves ref of repo with git ls-remote, in the environment ansible-galaxy gets: a
// branch first, then a tag (its peeled commit when annotated), and the default branch for an empty
// ref.
func (in GalaxyInstall) lsRemote(ctx context.Context, repo, ref string) (string, error) {
	if repo == "" || strings.HasPrefix(repo, "-") {
		return "", fmt.Errorf("%q is not a repository", repo)
	}
	ctx, cancel := context.WithTimeout(ctx, gitResolveTimeout)
	defer cancel()
	want := []string{"HEAD"}
	if ref != "" {
		want = []string{"refs/heads/" + ref, "refs/tags/" + ref + "^{}", "refs/tags/" + ref}
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"ls-remote", "--", repo}, want...)...) //nolint:gosec // G204: git ls-remote of the job's own requirement
	cmd.Env = append(append(ansible.InheritedEnv(), in.Env...), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = galaxyKillGrace
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s: %w", repo, err)
	}
	found := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			found[fields[1]] = fields[0]
		}
	}
	for _, w := range want {
		if commit := found[w]; fullCommit.MatchString(commit) {
			return commit, nil
		}
	}
	return "", fmt.Errorf("%s has no branch or tag %q", repo, ref)
}

// validSegment keeps a name or version out of the paths it would escape.
func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

// extractTarGz unpacks a verified archive into dir, refusing any entry that would leave it.
func extractTarGz(path, dir string) error {
	f, err := os.Open(path) //nolint:gosec // G304: the job's own cache scratch
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	root := filepath.Clean(dir) + string(os.PathSeparator)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, h.Name) //nolint:gosec // G305: checked against root below
		if !strings.HasPrefix(target+string(os.PathSeparator), root) {
			return fmt.Errorf("the archive entry %s leaves its directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: inside dir
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, io.LimitReader(tr, 256<<20))
			if err := errors.Join(copyErr, out.Close()); err != nil {
				return err
			}
		}
	}
}
