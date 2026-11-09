// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
)

// dockerConfig is the part of a Docker config file the launcher reads.
type dockerConfig struct {
	Auths map[string]struct {
		Auth          string `json:"auth"`
		Username      string `json:"username"`
		Password      string `json:"password"` //nolint:gosec // G117: a registry credential the launcher pulls with
		IdentityToken string `json:"identitytoken"`
	} `json:"auths"`
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// dockerConfigFiles are the files raw (RUNNER_DOCKER_CONFIG) names, comma separated: each entry a
// file, or a directory whose visible .json files are read in name order (a mounted Secret per
// file, as the chart projects them).
func dockerConfigFiles(raw string) ([]string, error) {
	var files []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		info, err := os.Stat(entry)
		if err != nil {
			return nil, fmt.Errorf("%s: reading %s: %w", EnvDockerConfig, entry, err)
		}
		if !info.IsDir() {
			files = append(files, entry)
			continue
		}
		dirents, err := os.ReadDir(entry)
		if err != nil {
			return nil, fmt.Errorf("%s: reading %s: %w", EnvDockerConfig, entry, err)
		}
		var inDir []string
		for _, d := range dirents {
			name := d.Name()
			if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
				continue
			}
			path := filepath.Join(entry, name)
			// A projected Secret's file is a link into a hidden directory: follow it.
			if st, err := os.Stat(path); err != nil || st.IsDir() {
				continue
			}
			inDir = append(inDir, path)
		}
		slices.Sort(inDir)
		files = append(files, inDir...)
	}
	return files, nil
}

// registryCredentialsFromEnv reads the launcher's own registry credentials from the Docker config
// files RUNNER_DOCKER_CONFIG names (one-launcher spec, A3, OD19). An auths entry gives a
// credential through auth (base64 of user:password), username and password, or identitytoken. A
// registry named in more than one file keeps the first file's entry. credsStore and credHelpers
// need a helper binary the launcher does not run, so they are reported as warnings, not read.
func registryCredentialsFromEnv(getenv func(string) string) (driver.RegistryCredentials, []string, error) {
	files, err := dockerConfigFiles(getenv(EnvDockerConfig))
	if err != nil || len(files) == 0 {
		return nil, nil, err
	}
	creds := driver.RegistryCredentials{}
	var warnings []string
	for _, path := range files {
		data, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file
		if err != nil {
			return nil, nil, fmt.Errorf("%s: reading %s: %w", EnvDockerConfig, path, err)
		}
		var cfg dockerConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, nil, fmt.Errorf("%s: %s is not a Docker config file: %w", EnvDockerConfig, path, err)
		}
		if cfg.CredsStore != "" {
			warnings = append(warnings, fmt.Sprintf("%s: credsStore in %s is not supported; put the credential inline in auths", EnvDockerConfig, path))
		}
		if len(cfg.CredHelpers) > 0 {
			warnings = append(warnings, fmt.Sprintf("%s: credHelpers in %s is not supported; put the credential inline in auths", EnvDockerConfig, path))
		}
		for _, key := range slices.Sorted(maps.Keys(cfg.Auths)) {
			entry := cfg.Auths[key]
			registry := driver.NormalizeRegistry(key)
			a := driver.RegistryAuth{Registry: registry, Username: entry.Username, Password: entry.Password, IdentityToken: entry.IdentityToken}
			if entry.Auth != "" {
				decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
				if err != nil {
					return nil, nil, fmt.Errorf("%s: the auth of %s in %s is not base64: %w", EnvDockerConfig, key, path, err)
				}
				user, pass, ok := strings.Cut(string(decoded), ":")
				if !ok {
					return nil, nil, fmt.Errorf("%s: the auth of %s in %s is not user:password", EnvDockerConfig, key, path)
				}
				a.Username, a.Password = user, pass
			}
			if a.Username == "" && a.IdentityToken == "" {
				continue
			}
			if _, seen := creds[registry]; !seen {
				creds[registry] = a
			}
		}
	}
	return creds, warnings, nil
}
