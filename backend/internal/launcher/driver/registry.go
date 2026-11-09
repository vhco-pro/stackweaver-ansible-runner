// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import "strings"

// RegistryCredentials are the launcher's own registry credentials, keyed by registry host
// (RUNNER_DOCKER_CONFIG, one-launcher spec, A3, OD19). The drivers pull the job images, the job
// image an executor is copied from and the firewall image with them, never a job's run image.
type RegistryCredentials map[string]RegistryAuth

// For is the credential for image's registry, nil when there is none.
func (c RegistryCredentials) For(image string) *RegistryAuth {
	a, ok := c[ImageRegistry(image)]
	if !ok {
		return nil
	}
	return &a
}

// ImageRegistry is the registry host of an image reference: its first path component when that
// names a host (it holds a dot or a port, or is localhost), else Docker Hub.
func ImageRegistry(image string) string {
	first, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io"
	}
	return NormalizeRegistry(first)
}

// ImageRepository is an image reference without its tag or digest, spelt with its registry host:
// ghcr.io/vhco-pro/stackweaver-opentofu-runner, and docker.io/library/alpine for Docker Hub's.
func ImageRepository(image string) string {
	name, _ := splitImageRef(image)
	path := name
	if first, rest, found := strings.Cut(name, "/"); found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		path = rest
	}
	registry := ImageRegistry(name)
	if registry == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return registry + "/" + path
}

// NormalizeRegistry turns a Docker config auths key into the registry host it names: no scheme,
// no path, and Docker Hub's several names as docker.io.
func NormalizeRegistry(key string) string {
	key = strings.TrimSpace(key)
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	key, _, _ = strings.Cut(key, "/")
	switch strings.ToLower(key) {
	case "index.docker.io", "registry-1.docker.io", "docker.io":
		return "docker.io"
	}
	return key
}
