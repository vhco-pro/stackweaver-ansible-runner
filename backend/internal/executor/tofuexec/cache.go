// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package tofuexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/executor/cachedesc"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/tofu"
)

// cacheUse is one run's use of its organization's cache (#933, D7): the release and the locked
// providers it takes from the cache, verified, and what it reports having installed beyond them.
type cacheUse struct {
	cache *executor.Cache
	desc  *runnercontract.CacheDescriptor
	// scratch is where fetched entries land; mirror is the packed filesystem mirror of providers.
	scratch string
	mirror  string
	// fromCache are the "source version" pairs of providers placed in the mirror.
	fromCache map[string]bool
	include   []string
	manifest  []runnercontract.CacheEntry
	// tofuVersion is the release taken from the cache, "" when none was.
	tofuVersion string
	// supplied are the providers placed from the cache, "source version", in order.
	supplied []string
	// fromAPI counts the entries fetched through the blob route rather than the launcher's
	// node-local copy.
	fromAPI int
}

func jobPlatform() string { return runtime.GOOS + "_" + runtime.GOARCH }

func newCacheUse(c *executor.Client, dir string, desc *runnercontract.CacheDescriptor, scratch string) *cacheUse {
	return &cacheUse{cache: &executor.Cache{Dir: dir, Client: c}, desc: desc, scratch: scratch, fromCache: map[string]bool{}}
}

// entries are the descriptor's entries of kind for this platform.
func (u *cacheUse) entries(kind runnercontract.CacheKind) []runnercontract.CacheEntryRef {
	if u.desc == nil {
		return nil
	}
	var out []runnercontract.CacheEntryRef
	for _, e := range u.desc.Entries {
		if e.Kind == kind && (e.Platform == "" || e.Platform == jobPlatform()) {
			out = append(out, e)
		}
	}
	return out
}

// tofuBinary is the run's engine: the cached release when the descriptor names one that verifies,
// otherwise core/tofu's resolution, whose download is reported.
func (u *cacheUse) tofuBinary(ctx context.Context, version, releases string) string {
	if version == "" {
		return resolveTofuBinary(version, releases) //nolint:contextcheck // core/tofu resolves once per job with its own deadline
	}
	if _, err := os.Stat(tofu.VersionedPath(version)); err != nil {
		for _, e := range u.entries(runnercontract.CacheKindTofu) {
			if e.Version != version {
				continue
			}
			zip := filepath.Join(u.scratch, "tofu-"+e.SHA256+".zip")
			src, err := u.cache.Fetch(ctx, e, zip)
			if err != nil || src == executor.SourceNone {
				if err != nil {
					logger.Warnf("The cached tofu %s could not be used: %v", version, err)
				}
				continue
			}
			path, err := tofu.InstallZip(version, zip)
			_ = os.Remove(zip)
			if err == nil {
				logger.Infof("tofu: installed opentofu %s from the organization's cache (%s)", version, src)
				u.tofuVersion = version
				if src == executor.SourceBlob {
					u.fromAPI++
				}
				return path
			}
			logger.Warnf("Installing the cached tofu %s failed: %v", version, err)
		}
	}
	path, dl, err := tofu.ResolveBinaryReporting(version, releases) //nolint:contextcheck // core/tofu resolves once per job with its own deadline
	if err != nil {
		logger.Errorf("FATAL: cannot get opentofu %s: %v", version, err)
		return tofu.VersionedPath(version)
	}
	if dl != nil {
		u.manifest = append(u.manifest, runnercontract.CacheEntry{
			Kind: runnercontract.CacheKindTofu, Name: "tofu", Version: dl.Version, Platform: dl.Platform, SHA256: dl.SHA256,
		})
	}
	return path
}

// placeProviders writes the descriptor's provider packages for this platform into a packed
// filesystem mirror, each verified, and records which providers it holds.
func (u *cacheUse) placeProviders(ctx context.Context) {
	osName, arch, _ := strings.Cut(jobPlatform(), "_")
	for _, e := range u.entries(runnercontract.CacheKindProvider) {
		parts := strings.Split(e.Name, "/")
		if len(parts) != 3 || !validSegment(parts[0]) || !validSegment(parts[1]) || !validSegment(parts[2]) || !validSegment(e.Version) {
			continue
		}
		if u.mirror == "" {
			u.mirror = filepath.Join(u.scratch, "providers")
		}
		dir := filepath.Join(u.mirror, parts[0], parts[1], parts[2])
		if err := os.MkdirAll(dir, 0o750); err != nil {
			continue
		}
		dst := filepath.Join(dir, fmt.Sprintf("terraform-provider-%s_%s_%s_%s.zip", parts[2], e.Version, osName, arch))
		src, err := u.cache.Fetch(ctx, e, dst)
		if err != nil || src == executor.SourceNone {
			continue
		}
		key := e.Name + " " + e.Version
		if !u.fromCache[key] {
			u.fromCache[key] = true
			u.include = append(u.include, e.Name)
			u.supplied = append(u.supplied, key)
			if src == executor.SourceBlob {
				u.fromAPI++
			}
		}
	}
}

// summary is the run log's line naming what the organization's cache supplied, "" when nothing.
func (u *cacheUse) summary() string {
	var parts []string
	if u.tofuVersion != "" {
		parts = append(parts, "OpenTofu "+u.tofuVersion)
	}
	for _, p := range u.supplied {
		parts = append(parts, "provider "+p)
	}
	if len(parts) == 0 {
		return ""
	}
	where := "From the organization's cache"
	if u.fromAPI == 0 {
		// Every entry came from the launcher's node-local copy; none crossed the API (decision 19).
		where += " (node-local copy)"
	}
	return where + ": " + strings.Join(parts, ", ") + "\n"
}

// validSegment keeps an entry's name and version out of the paths they would escape.
func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

// reportProviders reports the providers the lock file pins that the cache did not supply, with
// the lock file's hashes, which the populator holds the package to.
func (u *cacheUse) reportProviders(terraformDir string) {
	data, err := os.ReadFile(filepath.Join(terraformDir, cachedesc.LockFileName)) //nolint:gosec // G304: the run's own working directory
	if err != nil {
		return
	}
	providers, err := cachedesc.ParseLockFile(data)
	if err != nil {
		return
	}
	for _, p := range providers {
		if p.Version == "" || len(p.ZipHashes()) == 0 || u.fromCache[p.Source+" "+p.Version] {
			continue
		}
		u.manifest = append(u.manifest, runnercontract.CacheEntry{
			Kind: runnercontract.CacheKindProvider, Name: p.Source, Version: p.Version, Platform: jobPlatform(), Hashes: p.Hashes,
		})
	}
}

// report is the manifest a successful run sends with its completion; nil when it installed
// nothing beyond its descriptor.
func (u *cacheUse) report() *runnercontract.CacheManifest {
	if u == nil || len(u.manifest) == 0 {
		return nil
	}
	return &runnercontract.CacheManifest{Entries: u.manifest}
}

// providerInstallation is the CLI configuration's provider_installation block: the cached
// providers from the packed filesystem mirror, every other one from the network mirror or its
// registry.
func providerInstallation(networkMirror, fsMirror string, include []string) string {
	if fsMirror == "" || len(include) == 0 {
		if networkMirror == "" {
			return ""
		}
		return cliConfig(networkMirror)
	}
	quoted := make([]string, len(include))
	for i, p := range include {
		quoted[i] = fmt.Sprintf("%q", p)
	}
	list := "[" + strings.Join(quoted, ", ") + "]"
	var b strings.Builder
	b.WriteString("provider_installation {\n")
	fmt.Fprintf(&b, "  filesystem_mirror {\n    path    = %q\n    include = %s\n  }\n", fsMirror, list)
	if networkMirror != "" {
		if !strings.HasSuffix(networkMirror, "/") {
			networkMirror += "/"
		}
		fmt.Fprintf(&b, "  network_mirror {\n    url     = %q\n    exclude = %s\n  }\n", networkMirror, list)
	} else {
		fmt.Fprintf(&b, "  direct {\n    exclude = %s\n  }\n", list)
	}
	b.WriteString("}\n")
	return b.String()
}
