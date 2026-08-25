package main

import "testing"

// testEncryptionKey is a throwaway 32-byte hex key so loadConfig's encryptionkey.Resolve
// succeeds instead of calling logger.Fatalf and killing the test binary.
const testEncryptionKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestLoadConfigDatabaseSSLMode guards the regression fixed in #707: the ansible-runner
// hardcoded SSLMode: "disable" and ignored DATABASE_SSLMODE, so it could never connect to a
// TLS-enforcing Postgres (Azure Database for PostgreSQL rejects it with "no pg_hba.conf entry
// ... no encryption") even though the chart injects the var correctly.
func TestLoadConfigDatabaseSSLMode(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{name: "defaults to disable when unset", env: "", want: "disable"},
		{name: "honors require", env: "require", want: "require"},
		{name: "honors verify-full", env: "verify-full", want: "verify-full"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENCRYPTION_KEY", testEncryptionKey)
			t.Setenv("DATABASE_SSLMODE", tt.env)

			config := loadConfig()
			if got := config.DatabaseSSLMode; got != tt.want {
				t.Errorf("loadConfig().DatabaseSSLMode = %q, want %q", got, tt.want)
			}

			// The original bug was at the call site, not in loadConfig: assert the value
			// actually reaches the repository config the runner dials Postgres with.
			if got := databaseConfig(config).SSLMode; got != tt.want {
				t.Errorf("databaseConfig().SSLMode = %q, want %q", got, tt.want)
			}
		})
	}
}
