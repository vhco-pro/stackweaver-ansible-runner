// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package id

import (
	"regexp"
	"strings"
	"testing"
)

func TestGenerate_NormalOperation(t *testing.T) {
	prefixes := []string{"ws", "run", "sv", "var", "varset", "cv"}

	for _, prefix := range prefixes {
		t.Run(prefix, func(t *testing.T) {
			id, err := Generate(prefix)
			if err != nil {
				t.Fatalf("Generate(%q) returned error: %v", prefix, err)
			}

			// Verify format: {prefix}-{16-char-random}
			expectedPrefix := prefix + "-"
			if !strings.HasPrefix(id, expectedPrefix) {
				t.Errorf("Generate(%q) = %q, expected prefix %q", prefix, id, expectedPrefix)
			}

			// Verify length: prefix + "-" + 16 chars
			expectedLen := len(prefix) + 1 + IDLength
			if len(id) != expectedLen {
				t.Errorf("Generate(%q) = %q, length = %d, expected %d", prefix, id, len(id), expectedLen)
			}

			// Verify random part is alphanumeric
			randomPart := id[len(prefix)+1:]
			if len(randomPart) != IDLength {
				t.Errorf("Generate(%q) random part length = %d, expected %d", prefix, len(randomPart), IDLength)
			}

			// Verify all characters are alphanumeric
			alphanumericRegex := regexp.MustCompile("^[A-Za-z0-9]+$")
			if !alphanumericRegex.MatchString(randomPart) {
				t.Errorf("Generate(%q) random part %q contains non-alphanumeric characters", prefix, randomPart)
			}
		})
	}
}

func TestGenerate_Format(t *testing.T) {
	testCases := []struct {
		name   string
		prefix string
	}{
		{"workspace", "ws"},
		{"run", "run"},
		{"state version", "sv"},
		{"variable", "var"},
		{"variable set", "varset"},
		{"config version", "cv"},
		{"empty prefix", ""},
		{"long prefix", "very-long-prefix-name"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := Generate(tc.prefix)
			if err != nil {
				t.Fatalf("Generate(%q) returned error: %v", tc.prefix, err)
			}

			// Verify format matches pattern: {prefix}-{16-alphanumeric-chars}
			expectedPattern := "^" + regexp.QuoteMeta(tc.prefix) + "-[A-Za-z0-9]{16}$"
			matched, err := regexp.MatchString(expectedPattern, id)
			if err != nil {
				t.Fatalf("Regex match error: %v", err)
			}
			if !matched {
				t.Errorf("Generate(%q) = %q, does not match pattern %q", tc.prefix, id, expectedPattern)
			}
		})
	}
}

func TestGenerate_Uniqueness(t *testing.T) {
	const numIDs = 1000
	prefix := "test"

	ids := make(map[string]bool, numIDs)
	for i := 0; i < numIDs; i++ {
		id, err := Generate(prefix)
		if err != nil {
			t.Fatalf("Generate(%q) iteration %d returned error: %v", prefix, i, err)
		}

		if ids[id] {
			t.Errorf("Generate(%q) produced duplicate ID at iteration %d: %q", prefix, i, id)
		}
		ids[id] = true
	}

	// Verify we got the expected number of unique IDs
	if len(ids) != numIDs {
		t.Errorf("Generate(%q) produced %d unique IDs, expected %d", prefix, len(ids), numIDs)
	}
}

func TestGenerate_BoundsChecking(t *testing.T) {
	// Test that bounds checks prevent panics
	// Generate many IDs to stress test the bounds checking logic
	const numIterations = 10000
	prefix := "test"

	for i := 0; i < numIterations; i++ {
		id, err := Generate(prefix)
		if err != nil {
			// Bounds check errors should never occur in normal operation
			// If they do, it indicates a bug
			if strings.Contains(err.Error(), "index out of range") {
				t.Fatalf("Generate(%q) iteration %d returned bounds error: %v", prefix, i, err)
			}
			// Other errors (like rand.Read failure) are acceptable
			t.Logf("Generate(%q) iteration %d returned non-bounds error: %v", prefix, i, err)
			continue
		}

		// Verify ID is valid
		if id == "" {
			t.Errorf("Generate(%q) iteration %d returned empty ID", prefix, i)
		}
		if len(id) < len(prefix)+1+IDLength {
			t.Errorf("Generate(%q) iteration %d returned ID with invalid length: %q", prefix, i, id)
		}
	}
}

func TestGenerate_NoPanics(t *testing.T) {
	// Test that Generate never panics with various inputs
	testCases := []struct {
		name   string
		prefix string
	}{
		{"empty prefix", ""},
		{"single char", "a"},
		{"normal prefix", "ws"},
		{"long prefix", strings.Repeat("a", 100)},
		{"prefix with numbers", "test123"},
		{"prefix with special chars", "test-prefix_with-chars"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Run multiple times to catch any panic conditions
			for i := 0; i < 100; i++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("Generate(%q) iteration %d panicked: %v", tc.prefix, i, r)
						}
					}()
					_, err := Generate(tc.prefix)
					if err != nil {
						// Errors are acceptable (e.g., rand.Read failure), panics are not
						return
					}
				}()
			}
		})
	}
}
