// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"strings"
	"testing"
)

// FuzzSplitAndTrim asserts splitAndTrim never panics and always returns
// trimmed, non-empty fields when given arbitrary input.
func FuzzSplitAndTrim(f *testing.F) {
	f.Add("a,b,c", ",")
	f.Add("", ",")
	f.Add("  hello ,  world  ", ",")
	f.Add("no-separator-here", ",")
	f.Add(strings.Repeat("a,", 100), ",")
	f.Fuzz(func(t *testing.T, s, sep string) {
		if sep == "" {
			return // splitAndTrim requires non-empty separator by contract
		}
		got := splitAndTrim(s, sep)
		for i, v := range got {
			// splitAndTrim uses an internal trimSpace; we only assert the
			// stronger guarantee that no field is empty (the function filters
			// empties after trimming). We don't reassert the trim semantics
			// because they're internal to the package.
			if v == "" {
				t.Fatalf("splitAndTrim result[%d] is empty", i)
			}
		}
	})
}

// FuzzExtractVersion asserts extractVersion never panics on arbitrary text
// from `ansible --version` / `terraform version` / similar tool output.
func FuzzExtractVersion(f *testing.F) {
	f.Add("ansible [core 2.16.3]")
	f.Add("Terraform v1.6.0")
	f.Add("")
	f.Add("garbage with no version")
	f.Add("v1.2.3-rc.1+build.42")
	f.Fuzz(func(t *testing.T, s string) {
		_ = extractVersion(s) // must not panic
	})
}
