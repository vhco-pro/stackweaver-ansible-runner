// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

var pipName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// PipVerifier promotes a Python package only when the job's requirements pinned it by hash: pip
// downloads it with --require-hashes, which refuses any file whose digest is not pinned (D7). A
// requirements file without hashes is installed into the job's own overlay and never promoted.
type PipVerifier struct {
	// Pip is the pip command; empty means "pip".
	Pip       string
	IndexURL  string
	TrustHost string
}

// Verify implements Verifier. e.Name is the distribution's name, e.Version its exact version and
// e.Hashes its "sha256:<hex>" pins.
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
	line := e.Name + "==" + e.Version
	for _, p := range pins {
		line += " --hash=sha256:" + p
	}
	reqs := filepath.Join(dir, "requirements.txt")
	if err := os.WriteFile(reqs, []byte(line+"\n"), 0o600); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "dist")
	args := []string{"download", "--no-deps", "--require-hashes", "--disable-pip-version-check", "-d", out, "-r", reqs}
	if v.IndexURL != "" {
		args = append(args, "--index-url", v.IndexURL)
	}
	if v.TrustHost != "" {
		args = append(args, "--trusted-host", v.TrustHost)
	}
	pip := v.Pip
	if pip == "" {
		pip = "pip"
	}
	cmd := exec.CommandContext(ctx, pip, args...) //nolint:gosec // G204: fixed pip arguments over a validated name
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("pip is not installed on this launcher: %w", err)
		}
		return nil, fmt.Errorf("%w: pip refused %s: %s", ErrPoisoned, line, strings.TrimSpace(buf.String()))
	}
	files, _ := os.ReadDir(out)
	if len(files) != 1 {
		return nil, fmt.Errorf("pip downloaded %d files for %s, want 1", len(files), line)
	}
	path := filepath.Join(out, files[0].Name())
	sum, size, err := fileDigest(path)
	if err != nil {
		return nil, err
	}
	// pip enforced the pin; checking it again keeps a wrong pip from being trusted blindly.
	if !slices.Contains(pins, sum) {
		return nil, fmt.Errorf("%w: %s hashes to %s, which the requirements do not pin", ErrPoisoned, files[0].Name(), sum)
	}
	return &Artifact{Path: path, SHA256: sum, Size: size, SourceURL: line, Verification: Verified}, nil
}
