// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/security/gitargs"
)

var gitCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GitRunner is the git the verifiers run.
type GitRunner interface {
	// TagCommit resolves a tag of a remote repository to the commit it points at.
	TagCommit(ctx context.Context, repo, tag string) (string, error)
	// Archive writes a gzipped tar of the repository at exactly commit to dst.
	Archive(ctx context.Context, repo, commit, dst string) error
}

// execGit is the launcher's git. It runs outside any job's sandbox, so it fetches over https only
// and consults no credential helper or configuration of the host, as the sync clone does.
type execGit struct {
	// protocols are the transports git may use; none means https only. Tests add file.
	protocols []string
}

func (g execGit) config() []string {
	protocols := g.protocols
	if len(protocols) == 0 {
		protocols = []string{"https"}
	}
	args := []string{"-c", "credential.helper=", "-c", "protocol.allow=never"}
	for _, p := range protocols {
		args = append(args, "-c", "protocol."+p+".allow=always")
	}
	return args
}

func gitEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

func (g execGit) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(g.config(), args...)...) //nolint:gosec // G204: fixed git subcommands over validated arguments
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (g execGit) TagCommit(ctx context.Context, repo, tag string) (string, error) {
	out, err := g.run(ctx, "", "ls-remote", "--tags", "--", repo, "refs/tags/"+tag, "refs/tags/"+tag+"^{}")
	if err != nil {
		return "", err
	}
	commit := ""
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// An annotated tag's peeled line names the commit itself.
		if strings.HasSuffix(fields[1], "^{}") || commit == "" {
			commit = fields[0]
		}
	}
	if !gitCommit.MatchString(commit) {
		return "", fmt.Errorf("the repository has no tag %s", tag)
	}
	return commit, nil
}

func (g execGit) Archive(ctx context.Context, repo, commit, dst string) error {
	work, err := os.MkdirTemp(filepath.Dir(dst), "clone-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	if _, err := g.run(ctx, work, "init", "-q"); err != nil {
		return err
	}
	if _, err := g.run(ctx, work, "fetch", "-q", "--depth=1", "--", repo, commit); err != nil {
		return err
	}
	got, err := g.run(ctx, work, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != commit {
		return fmt.Errorf("%w: the repository served %s for %s", ErrPoisoned, strings.TrimSpace(got), commit)
	}
	_, err = g.run(ctx, work, "archive", "--format=tar.gz", "--prefix=source/", "-o", dst, commit)
	return err
}

// GitVerifier promotes a git-sourced collection or role at exactly the commit the job reported
// (D7): ansible-galaxy verifies nothing for git sources, so the entry is keyed by the commit, and
// the populator fetches that commit and no other.
type GitVerifier struct {
	Git GitRunner
}

// Verify implements Verifier. e.Name is the repository URL and e.Version the full commit SHA.
func (v *GitVerifier) Verify(ctx context.Context, e runnercontract.CacheEntry, dir string) (*Artifact, error) {
	if !gitCommit.MatchString(e.Version) {
		return nil, fmt.Errorf("a git source is promoted only at an exact commit, not %q", e.Version)
	}
	// The job reports the repository, so only an https URL of the form the sync clone accepts is
	// fetched. A source the populator does not fetch still installs; it is just not cached.
	if !gitargs.HTTPSCloneURLRe.MatchString(e.Name) {
		return nil, fmt.Errorf("a git source is promoted only from an https repository, not %q", e.Name)
	}
	git := v.Git
	if git == nil {
		git = execGit{}
	}
	dst := filepath.Join(dir, "source.tar.gz")
	if err := git.Archive(ctx, e.Name, e.Version, dst); err != nil {
		return nil, err
	}
	sum, size, err := fileDigest(dst)
	if err != nil {
		return nil, err
	}
	return &Artifact{Path: dst, SHA256: sum, Size: size, SourceURL: e.Name + "@" + e.Version, Verification: Verified}, nil
}

func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the verifier's scratch directory
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the verifier's scratch directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return nil, errors.Join(err)
	}
	return b, nil
}
