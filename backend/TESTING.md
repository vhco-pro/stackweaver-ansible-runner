<!-- Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details. -->

# Registry API Testing Guide

This guide explains how to run integration tests for the Terraform Registry API endpoints.

## Overview

The registry API includes comprehensive integration tests that can be run against a live database. Tests cover:
- Module listing and search
- Module version management
- Module publishing (tarball upload)
- Provider creation
- Provider platform publishing
- Provider platform publishing with GPG signing

## Prerequisites

1. **PostgreSQL Database**: Tests require a PostgreSQL database (can be the same as your development database)
2. **Go 1.21+**: Required for running Go tests
3. **Optional: GPG**: For GPG signing tests (tests will skip if GPG is not available)

## Running Tests

### Option 1: Using Test Script (Recommended)

```bash
cd backend
./test_registry.sh
```

The script will:
- Use `TEST_DATABASE_URL` if set, otherwise default to local database
- Run all provider publishing tests
- Provide colored output for easy reading

### Option 2: Manual Test Execution

#### Set Database URL

```bash
# Use live database
export TEST_DATABASE_URL="postgres://iac:iac_password@localhost:5432/iac_platform?sslmode=disable"

# Or use a dedicated test database
export TEST_DATABASE_URL="postgres://user:pass@localhost:5432/test_db?sslmode=disable"
```

#### Run Specific Tests

```bash
# Module tests
go test -v ./internal/api/v2/handlers -run TestListModules
go test -v ./internal/api/v2/handlers -run TestGetModuleVersions
go test -v ./internal/api/v2/handlers -run TestPublishModuleVersion

# Provider tests
go test -v ./internal/api/v2/handlers -run TestCreateProvider
go test -v ./internal/api/v2/handlers -run TestPublishProviderPlatform
go test -v ./internal/api/v2/handlers -run TestPublishProviderPlatformWithGPG

# Run all registry tests
go test -v ./internal/api/v2/handlers -run "Test.*"
```

#### Run All Tests

```bash
go test -v ./internal/api/v2/handlers/...
```

## Test Structure

### Test Database Setup

Tests use `setupTestDB()` or `setupTestDBForProvider()` functions that:
- Connect to PostgreSQL database (via `TEST_DATABASE_URL`)
- Automatically run migrations for required models
- Create test organizations and users
- Clean up test data after tests complete

### Test Isolation

Each test:
- Creates unique test data (organizations, providers, modules)
- Uses unique names (UUID-based) to avoid conflicts
- Cleans up after itself (drops test data)

### Mock Storage

Tests use `registry.NewMockStorage()` for in-memory storage, so:
- No MinIO/S3 setup required for tests
- Tests are faster and more isolated
- Storage operations are verified without external dependencies

## Test Coverage

### Module Registry Tests

- ✅ `TestListModules` - Verifies module listing with pagination
- ✅ `TestGetModuleVersions` - Verifies version listing and sorting
- ✅ `TestPublishModuleVersion` - Verifies module version publishing from tarball

### Provider Registry Tests

- ✅ `TestCreateProvider` - Verifies provider creation endpoint
- ✅ `TestPublishProviderPlatform` - Verifies provider binary upload
- ✅ `TestPublishProviderPlatformWithGPG` - Verifies GPG signing (optional, skips if GPG unavailable)

## CI/CD Integration

Tests are designed to work in CI/CD pipelines:

```yaml
# Example GitHub Actions workflow
- name: Run Registry Tests
  env:
    TEST_DATABASE_URL: postgres://user:pass@localhost:5432/test_db?sslmode=disable
  run: |
    go test -v ./internal/api/v2/handlers -run "Test.*"
```

**Note**: If `TEST_DATABASE_URL` is not set, tests will skip automatically (useful for local development without database).

## Troubleshooting

### Tests Skip with "TEST_DATABASE_URL not set"

Set the environment variable:
```bash
export TEST_DATABASE_URL="postgres://user:pass@localhost:5432/dbname?sslmode=disable"
```

### Database Connection Errors

1. Ensure PostgreSQL is running
2. Verify connection string is correct
3. Check database permissions
4. Ensure database exists

### GPG Tests Fail

GPG signing tests may fail if:
- GPG is not installed in the test environment
- GPG keys are not properly configured
- This is expected and tests will log a warning

### Test Data Conflicts

If tests fail due to existing data:
- Use a dedicated test database
- Or ensure test cleanup runs properly
- Tests use UUID-based names to minimize conflicts

## Writing New Tests

### Example Test Structure

```go
func TestMyNewFeature(t *testing.T) {
    db := setupTestDBForProvider(t)
    defer func() {
        // Cleanup
        db.Exec("DELETE FROM my_table")
    }()

    org := setupTestOrgForProvider(t, db)
    
    // Your test code here
    // ...
    
    // Assertions
    if condition != expected {
        t.Errorf("Expected X, got Y")
    }
}
```

### Best Practices

1. **Use `setupTestDB()` helpers** - They handle migrations and cleanup
2. **Create unique test data** - Use UUIDs or timestamps in names
3. **Clean up after tests** - Use `defer` to ensure cleanup runs
4. **Skip if dependencies unavailable** - Use `t.Skipf()` for optional features
5. **Use mock storage** - `registry.NewMockStorage()` for storage operations

## Test Output

Tests provide verbose output when run with `-v` flag:
- Test names and status
- Request/response details
- Error messages with context
- Cleanup confirmation

Example output:
```
=== RUN   TestCreateProvider
--- PASS: TestCreateProvider (0.123s)
=== RUN   TestPublishProviderPlatform
--- PASS: TestPublishProviderPlatform (0.456s)
```

