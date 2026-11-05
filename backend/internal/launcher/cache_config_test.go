// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// The node-local cache is on by default, under the user's cache directory, bounded by
// cache.local_size_limit; RUNNER_CACHE_DIR=off switches it off (#933, D7, decision 19).
func TestConfigFromEnvCache(t *testing.T) {
	base := map[string]string{EnvServer: "http://api", EnvToken: "swa-x"}
	cfg, err := ConfigFromEnv(Binary{Engine: "tofu"}, envOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.CacheDir, filepath.Join("stackweaver", "launcher-cache")) || cfg.CacheLimit != 20<<30 {
		t.Errorf("default cache = %q limit %d, want the user cache directory and 20Gi", cfg.CacheDir, cfg.CacheLimit)
	}

	set := map[string]string{EnvCacheDir: "/var/cache/stackweaver-launcher", EnvCacheVolume: "stackweaver-launcher-cache", EnvCacheLimit: "512Mi"}
	for k, v := range base {
		set[k] = v
	}
	cfg, err = ConfigFromEnv(Binary{Engine: "tofu"}, envOf(set))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CacheDir != "/var/cache/stackweaver-launcher" || cfg.CacheVolume != "stackweaver-launcher-cache" || cfg.CacheLimit != 512<<20 {
		t.Errorf("cache = %+v", cfg)
	}

	set[EnvCacheDir] = "off"
	if cfg, _ = ConfigFromEnv(Binary{Engine: "tofu"}, envOf(set)); cfg.CacheDir != "" {
		t.Errorf("RUNNER_CACHE_DIR=off left %q", cfg.CacheDir)
	}
	set[EnvCacheDir] = "/x"
	set[EnvCacheLimit] = "lots"
	if _, err := ConfigFromEnv(Binary{Engine: "tofu"}, envOf(set)); err == nil {
		t.Error("an unreadable size limit was accepted")
	}
}
