// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package tofuexec

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/tofu"
)

// ExtractTarGz unpacks a run's configuration tarball into destDir with the executor's shared,
// bounded extraction: an entry over the per-file limit fails the run instead of being cut short.
func ExtractTarGz(data []byte, destDir string) error {
	return executor.ExtractTarGz(bytes.NewReader(data), destDir)
}

// replaceRemoteBackend replaces remote/cloud backend configs with local backend
// to prevent the self-hosted runner from creating nested remote runs.
// This uses brace-counting to properly remove the entire block (including body and closing brace).
func replaceRemoteBackend(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tf") {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		content, err := os.ReadFile(filePath) //nolint:gosec // G304: filePath is constructed from trusted directory listing
		if err != nil {
			continue
		}
		contentStr := string(content)
		modified := contentStr

		// Remove cloud { ... } blocks (used by TFC/TFE)
		modified = RemoveHCLBlock(modified, "cloud {")
		// Remove backend "remote" { ... } blocks
		modified = RemoveHCLBlock(modified, `backend "remote" {`)

		if modified != contentStr {
			_ = os.WriteFile(filePath, []byte(modified), 0o600) //nolint:gosec // G703: filePath is constructed from trusted directory listing via os.ReadDir
			logger.Infof("Replaced remote backend in %s", entry.Name())
		}
	}
}

// RemoveHCLBlock finds a block starting with the given opener (e.g. "cloud {" or `backend "remote" {`)
// and removes the entire block including all nested braces and the closing brace.
// This handles nested blocks correctly via brace counting.
func RemoveHCLBlock(content, opener string) string {
	for {
		idx := strings.Index(content, opener)
		if idx == -1 {
			return content
		}

		// Find the start of the block opening (include any leading whitespace on the same line)
		blockStart := idx
		for blockStart > 0 && content[blockStart-1] != '\n' {
			blockStart--
		}

		// Count braces to find the matching closing brace
		braceCount := 0
		pos := idx
		foundOpen := false
		for pos < len(content) {
			if content[pos] == '{' {
				braceCount++
				foundOpen = true
			} else if content[pos] == '}' {
				braceCount--
				if foundOpen && braceCount == 0 {
					// Found the matching closing brace
					blockEnd := pos + 1
					// Include trailing newline if present
					if blockEnd < len(content) && content[blockEnd] == '\n' {
						blockEnd++
					}
					// Replace block with a comment
					replacement := "  # Backend removed by StackWeaver agent runner\n  backend \"local\" {}\n"
					content = content[:blockStart] + replacement + content[blockEnd:]
					break
				}
			}
			pos++
		}

		if !foundOpen || braceCount != 0 {
			// Malformed block - bail to avoid infinite loop
			return content
		}
	}
}

// lockedWriter serializes writes to an io.Writer that is not safe for concurrent use.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// jobOutputWriters returns the writers for a tofu process's stdout and stderr. Both feed the
// same streamer and output buffer, which are not goroutine-safe (AUD-028): os/exec only shares
// one copy goroutine when Stdout and Stderr are the identical value, and here they differ
// (each also tees to its own console stream), so exec copies the two pipes on two goroutines.
// The shared sinks therefore sit behind one mutex; only the console tees stay unsynchronized,
// as os.Stdout and os.Stderr are safe for concurrent writes.
func jobOutputWriters(streamer, fullOutput, stdout, stderr io.Writer) (io.Writer, io.Writer) {
	shared := &lockedWriter{w: io.MultiWriter(streamer, fullOutput)}
	return io.MultiWriter(shared, stdout), io.MultiWriter(shared, stderr)
}

// planMetadata holds parsed resource counts from a plan JSON
type planMetadata struct {
	Additions     int
	Changes       int
	Destructions  int
	OutputChanges int
	HasChanges    bool
}

// parsePlanJSON extracts resource and output change counts from terraform show -json output
func parsePlanJSON(planJSON string) planMetadata {
	var plan map[string]interface{}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		logger.Warnf("Failed to parse plan JSON: %v", err)
		return planMetadata{}
	}

	meta := planMetadata{}

	// Count resource changes
	if resourceChanges, ok := plan["resource_changes"].([]interface{}); ok {
		for _, rc := range resourceChanges {
			change, ok := rc.(map[string]interface{})
			if !ok {
				continue
			}
			changeDetail, ok := change["change"].(map[string]interface{})
			if !ok {
				continue
			}
			actions, ok := changeDetail["actions"].([]interface{})
			if !ok {
				continue
			}
			for _, action := range actions {
				actionStr, ok := action.(string)
				if !ok {
					continue
				}
				switch actionStr {
				case "create":
					meta.Additions++
				case "update":
					meta.Changes++
				case "delete":
					meta.Destructions++
				}
			}
		}
	}

	// Count output changes (output_changes is a map of name -> change object)
	if outputChanges, ok := plan["output_changes"].(map[string]interface{}); ok {
		for _, oc := range outputChanges {
			changeMap, ok := oc.(map[string]interface{})
			if !ok {
				continue
			}
			actions, ok := changeMap["actions"].([]interface{})
			if !ok {
				continue
			}
			for _, a := range actions {
				if actionStr, ok := a.(string); ok && actionStr != "no-op" {
					meta.OutputChanges++
					break
				}
			}
		}
	}

	meta.HasChanges = meta.Additions > 0 || meta.Changes > 0 || meta.Destructions > 0 || meta.OutputChanges > 0
	return meta
}

// resolveTofuBinary finds the exact OpenTofu binary for the requested version, downloading it if
// needed. Resolution and download live in core/tofu so this agent path and the library path in
// core/plugins/opentofu share one implementation - historically this file carried a near-duplicate
// downloader that never verified checksums (AUD-058 hardened only the other copy).
//
// The version must be set on the workspace or org before a run is created.
func resolveTofuBinary(requestedVersion, releasesURL string) string {
	if requestedVersion == "" {
		logger.Errorf("FATAL: no opentofu version specified -- version must be set on workspace or organization")
		return "tofu-version-not-set" // will fail with a clear error
	}

	path, err := tofu.ResolveBinaryFrom(requestedVersion, releasesURL)
	if err != nil {
		logger.Errorf("FATAL: cannot get opentofu %s: %v", requestedVersion, err)
		// Return the canonical versioned path so the command fails with "binary not found"
		// rather than silently executing a different version.
		return tofu.VersionedPath(requestedVersion)
	}
	return path
}

// TofuVersion reports the version of the tofu binary on PATH, for runner registration.
// OpenTofu keeps Terraform's `version -json` schema, including the "terraform_version" key.
func TofuVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, tofu.BinaryName, "version", "-json") //nolint:gosec // G204: BinaryName is a package constant, args are literals
	output, err := cmd.Output()
	if err != nil {
		// Try without the -json flag
		cmd = exec.CommandContext(ctx, tofu.BinaryName, "version") //nolint:gosec // G204: BinaryName is a package constant, args are literals
		output, err = cmd.Output()
		if err != nil {
			return "unknown"
		}
		// Parse the first line: "OpenTofu vX.Y.Z"
		lines := strings.Split(string(output), "\n")
		if len(lines) > 0 {
			parts := strings.Fields(lines[0])
			if len(parts) >= 2 {
				return strings.TrimPrefix(parts[1], "v")
			}
		}
		return "unknown"
	}

	var versionInfo struct {
		TerraformVersion string `json:"terraform_version"`
	}
	if err := json.Unmarshal(output, &versionInfo); err != nil {
		return "unknown"
	}
	return versionInfo.TerraformVersion
}
