// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package registry

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidateSemanticVersion validates a semantic version string (strict, no pre-release)
// Format: MAJOR.MINOR.PATCH (e.g., "1.0.0", "2.1.3")
// Rejects: pre-release versions (e.g., "1.0.0-alpha.1", "1.0.0-beta")
func ValidateSemanticVersion(version string) error {
	// Remove 'v' prefix if present (e.g., "v1.0.0" -> "1.0.0")
	version = strings.TrimPrefix(version, "v")

	// Strict semver pattern: MAJOR.MINOR.PATCH
	// No pre-release, no build metadata
	pattern := `^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`
	matched, err := regexp.MatchString(pattern, version)
	if err != nil {
		return fmt.Errorf("invalid version format: %w", err)
	}

	if !matched {
		return fmt.Errorf("invalid semantic version: %s (must be MAJOR.MINOR.PATCH, no pre-release versions allowed)", version)
	}

	return nil
}

// ExtractVersionFromTag extracts version from Git tag, removing 'v' prefix if present
// Examples: "v1.0.0" -> "1.0.0", "2.1.3" -> "2.1.3"
func ExtractVersionFromTag(tag string) string {
	return strings.TrimPrefix(tag, "v")
}

// NormalizeVersion normalizes a version string (removes 'v' prefix)
func NormalizeVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}
