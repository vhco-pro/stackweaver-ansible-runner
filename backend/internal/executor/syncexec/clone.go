// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package syncexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/security/gitargs"
)

// cloner clones a repository for a work item, holding the connection's credential in memory only
// (#933, D2, decision 18, AC68): a token reaches git through a one-shot GIT_ASKPASS helper that
// asks this process over a socket, and an SSH deploy key and the pinned host key reach ssh as
// memory files git inherits. Neither is in an argument, the environment or a file of the work
// directory.
type cloner struct {
	// askpass is the helper binary git runs: this executable, which answers as AskpassMain when
	// EnvAskpassSocket is set.
	askpass string
	// ssh is the ssh client GIT_SSH_COMMAND runs.
	ssh string
	// env is the clone's base environment: the inherited allowlist and the mirror block.
	env []string
	// secretDir is the job-private directory the askpass socket is created in.
	secretDir string
	// observe receives every git argument list and environment, for a test to read.
	observe func(args, env []string)
}

// sshCommand is GIT_SSH_COMMAND for an SSH clone: the key and the known hosts are the memory files
// at fd 3 and 4, nothing else is trusted, and an unknown or changed host key fails the clone.
func (c *cloner) sshCommand() string {
	return c.ssh + " -i /dev/fd/3 -o IdentitiesOnly=yes -o IdentityAgent=none" +
		" -o UserKnownHostsFile=/dev/fd/4 -o GlobalKnownHostsFile=/dev/null" +
		" -o StrictHostKeyChecking=yes -o BatchMode=yes"
}

// clone clones src's branch into dest and returns the commit it resolved. When src names a
// Commit, that commit is checked out instead of the branch head (#938): the clone is shallow and
// the branch may have moved on since the commit was requested, so the commit is fetched by id,
// or, when the host refuses a fetch by id, the fetch ref (or the branch) is fetched with its full
// history; a commit still absent fails the clone.
func (c *cloner) clone(ctx context.Context, src *runnercontract.CloneSource, dest string, out io.Writer) (string, error) {
	if src == nil || src.URL == "" {
		return "", errors.New("the work item names no repository")
	}
	branch := src.Branch
	if branch == "" {
		branch = "main"
	}
	if !gitargs.RefNameRe.MatchString(branch) {
		return "", fmt.Errorf("refusing branch %q", branch)
	}
	if src.Commit != "" && gitargs.CommitSHARe.FindString(src.Commit) != src.Commit {
		return "", fmt.Errorf("refusing commit %q", src.Commit)
	}
	if src.FetchRef != "" && gitargs.RefNameRe.FindString(src.FetchRef) != src.FetchRef {
		return "", fmt.Errorf("refusing fetch ref %q", src.FetchRef)
	}
	env := append([]string{}, c.env...)
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	var extra []*os.File
	// credentials returns the environment for one git command that may authenticate. The askpass
	// server answers a single command, so each such command gets its own; release closes it.
	credentials := func() (cmdEnv []string, release func(), err error) {
		if !gitargs.HTTPSCloneURLRe.MatchString(src.URL) || src.Token == "" {
			return env, func() {}, nil
		}
		srv, err := serveAskpass(ctx, c.secretDir, src.Username, src.Token)
		if err != nil {
			return nil, nil, err
		}
		cmdEnv = append(append([]string{}, env...), "GIT_ASKPASS="+c.askpass, EnvAskpassSocket+"="+srv.path)
		return cmdEnv, srv.Close, nil
	}
	switch {
	case gitargs.HTTPSCloneURLRe.MatchString(src.URL):
	case gitargs.SSHCloneURLRe.MatchString(src.URL):
		if strings.TrimSpace(src.KnownHosts) == "" {
			return "", errors.New("an SSH clone needs the connection's pinned host key")
		}
		if src.SSHPrivateKey == "" {
			return "", errors.New("an SSH clone needs the connection's deploy key")
		}
		key, err := memFile("deploy-key", []byte(strings.TrimRight(src.SSHPrivateKey, "\n")+"\n"))
		if err != nil {
			return "", err
		}
		defer func() { _ = key.Close() }()
		hosts, err := memFile("known-hosts", []byte(src.KnownHosts))
		if err != nil {
			return "", err
		}
		defer func() { _ = hosts.Close() }()
		extra = []*os.File{key, hosts}
		env = append(env, "GIT_SSH_COMMAND="+c.sshCommand())
	default:
		return "", fmt.Errorf("refusing repository URL %q", src.URL)
	}

	// No credential helper of the image is consulted, and the clone keeps no credential.
	args := []string{"-c", "credential.helper=", "clone", "--depth", "1", "--single-branch", "--branch", branch, "--", src.URL, dest}
	cloneEnv, release, err := credentials()
	if err != nil {
		return "", err
	}
	defer release()
	if c.observe != nil {
		c.observe(args, cloneEnv)
	}
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: the URL and branch are validated above; positionals after --
	executor.BoundOrphanPipes(cmd)
	cmd.Env, cmd.ExtraFiles = cloneEnv, extra
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = io.MultiWriter(out, &stderr)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cloning %s: %w: %s", src.URL, err, strings.TrimSpace(stderr.String()))
	}
	rev := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	executor.BoundOrphanPipes(rev)
	rev.Dir, rev.Env = dest, env
	commit, err := rev.Output()
	if err != nil {
		return "", fmt.Errorf("reading the cloned commit: %w", err)
	}
	head := strings.TrimSpace(string(commit))
	if src.Commit == "" || src.Commit == head {
		return head, nil
	}
	// git runs a command in the clone with the same credential arrangement as the clone.
	git := func(args ...string) error {
		cmdEnv, release, err := credentials()
		if err != nil {
			return err
		}
		defer release()
		full := append([]string{"-c", "credential.helper="}, args...)
		cmd := exec.CommandContext(ctx, "git", full...) //nolint:gosec // G204: the commit and refs are validated above
		executor.BoundOrphanPipes(cmd)
		cmd.Dir, cmd.Env, cmd.ExtraFiles = dest, cmdEnv, extra
		var stderr bytes.Buffer
		cmd.Stdout = out
		cmd.Stderr = io.MultiWriter(out, &stderr)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	if err := git("fetch", "-q", "--depth", "1", "origin", src.Commit); err != nil {
		ref := src.FetchRef
		if ref == "" {
			ref = branch
		}
		_, _ = fmt.Fprintf(out, "The host refused a fetch of %s by id; fetching %s with its history\n", src.Commit, ref)
		if err := git("fetch", "-q", "--unshallow", "origin", ref); err != nil {
			return "", fmt.Errorf("fetching %s for commit %s: %w", ref, src.Commit, err)
		}
	}
	if err := git("checkout", "-q", "--detach", src.Commit); err != nil {
		return "", fmt.Errorf("checking out commit %s: %w", src.Commit, err)
	}
	return src.Commit, nil
}
