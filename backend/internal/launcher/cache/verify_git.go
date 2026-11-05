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
)

var gitCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GitRunner is the git the verifiers run.
type GitRunner interface {
	// TagCommit resolves a tag of a remote repository to the commit it points at.
	TagCommit(ctx context.Context, repo, tag string) (string, error)
	// Archive writes a gzipped tar of the repository at exactly commit to dst.
	Archive(ctx context.Context, repo, commit, dst string) error
}

type execGit struct{}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: fixed git subcommands over validated arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (execGit) TagCommit(ctx context.Context, repo, tag string) (string, error) {
	out, err := runGit(ctx, "", "ls-remote", "--tags", "--", repo, "refs/tags/"+tag, "refs/tags/"+tag+"^{}")
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

func (execGit) Archive(ctx context.Context, repo, commit, dst string) error {
	work, err := os.MkdirTemp(filepath.Dir(dst), "clone-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	if _, err := runGit(ctx, work, "init", "-q"); err != nil {
		return err
	}
	if _, err := runGit(ctx, work, "fetch", "-q", "--depth=1", "--", repo, commit); err != nil {
		return err
	}
	got, err := runGit(ctx, work, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != commit {
		return fmt.Errorf("%w: the repository served %s for %s", ErrPoisoned, strings.TrimSpace(got), commit)
	}
	_, err = runGit(ctx, work, "archive", "--format=tar.gz", "--prefix=source/", "-o", dst, commit)
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
	if strings.HasPrefix(e.Name, "-") || e.Name == "" {
		return nil, fmt.Errorf("%q is not a repository", e.Name)
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
