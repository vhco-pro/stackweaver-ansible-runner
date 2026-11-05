// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package identity turns the platform-minted per-run workload-identity tokens a job's artifacts
// carry into what each cloud SDK reads (#933, D8): the AWS web-identity token file, the GCP
// credential configuration, a Vault client token from a JWT login. Both engines' executors and the
// queue runner call it, so a job authenticates the same way on every path.
package identity
