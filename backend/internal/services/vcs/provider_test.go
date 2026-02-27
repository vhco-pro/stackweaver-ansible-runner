// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs_test

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G401: Testing Azure DevOps HMAC-SHA1 webhook validation
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/services/vcs"
)

// =============================================================================
// Provider Registry Tests
// =============================================================================

func TestProviderRegistry_GetProvider(t *testing.T) {
	registry := vcs.NewProviderRegistry(nil, nil, nil) // nil managers are fine for type resolution

	tests := []struct {
		name        string
		provider    models.VCSProvider
		wantErr     bool
		errContains string
	}{
		{"github", models.VCSProviderGitHub, false, ""},
		{"azure_devops", models.VCSProviderAzureDevOps, false, ""},
		{"gitlab", models.VCSProviderGitLab, false, ""},
		{"bitbucket", models.VCSProviderBitbucket, false, ""},
		{"unsupported", models.VCSProvider("unsupported"), true, "unsupported VCS provider"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &models.VCSConnection{Provider: tt.provider}
			provider, err := registry.GetProvider(conn)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Fatalf("error %q should contain %q", err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider == nil {
				t.Fatal("expected non-nil provider")
			}
		})
	}
}

// =============================================================================
// GitHub Provider Tests (no external API calls)
// =============================================================================

func TestGitHubProvider_BuildCloneURL(t *testing.T) {
	provider := &vcs.GitHubProvider{}

	tests := []struct {
		name     string
		token    string
		repoPath string
		want     string
	}{
		{
			"with token",
			"ghp_abc123",
			"owner/repo",
			"https://x-access-token:ghp_abc123@github.com/owner/repo.git",
		},
		{
			"without token",
			"",
			"owner/repo",
			"https://github.com/owner/repo.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provider.BuildCloneURL(nil, tt.token, tt.repoPath)
			if got != tt.want {
				t.Errorf("BuildCloneURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitHubProvider_ValidateWebhook(t *testing.T) {
	provider := &vcs.GitHubProvider{}

	t.Run("empty secret passes", func(t *testing.T) {
		if err := provider.ValidateWebhook([]byte("payload"), "anything", ""); err != nil {
			t.Fatalf("expected nil error for empty secret, got: %v", err)
		}
	})

	t.Run("valid signature passes", func(t *testing.T) {
		secret := "my-webhook-secret" //nolint:gosec
		payload := []byte(`{"ref":"refs/heads/main"}`)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(payload)
		sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

		if err := provider.ValidateWebhook(payload, sig, secret); err != nil {
			t.Fatalf("expected valid signature to pass, got: %v", err)
		}
	})

	t.Run("invalid signature fails", func(t *testing.T) {
		if err := provider.ValidateWebhook([]byte("payload"), "sha256=bad", "secret"); err == nil {
			t.Fatal("expected error for invalid signature")
		}
	})
}

func TestGitHubProvider_ParseWebhookPayload_Push(t *testing.T) {
	provider := &vcs.GitHubProvider{}

	payload := []byte(`{
		"ref": "refs/heads/main",
		"after": "abc123def456",
		"repository": {"full_name": "octocat/hello-world"},
		"commits": [
			{
				"id": "abc123def456",
				"added": ["new-file.tf"],
				"modified": ["main.tf"],
				"removed": [],
				"author": {"name": "Octocat", "email": "octocat@github.com"}
			}
		],
		"pusher": {"name": "Octocat", "email": "octocat@github.com"}
	}`)

	wp, err := provider.ParseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertEqual(t, "EventType", wp.EventType, "push")
	assertEqual(t, "Repository", wp.Repository, "octocat/hello-world")
	assertEqual(t, "Branch", wp.Branch, "main")
	assertEqual(t, "Commit", wp.Commit, "abc123def456")
	assertEqual(t, "Committer", wp.Committer, "Octocat <octocat@github.com>")

	if len(wp.ChangedFiles) != 2 {
		t.Fatalf("expected 2 changed files, got %d: %v", len(wp.ChangedFiles), wp.ChangedFiles)
	}
}

func TestGitHubProvider_ParseWebhookPayload_PullRequest(t *testing.T) {
	provider := &vcs.GitHubProvider{}

	payload := []byte(`{
		"action": "opened",
		"number": 42,
		"pull_request": {
			"number": 42,
			"head": {"ref": "feature-branch", "sha": "headsha123"},
			"base": {"ref": "main"}
		},
		"repository": {"full_name": "octocat/hello-world"}
	}`)

	wp, err := provider.ParseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertEqual(t, "EventType", wp.EventType, "pull_request")
	assertEqual(t, "Repository", wp.Repository, "octocat/hello-world")
	assertEqual(t, "PRNumber", itoa(wp.PRNumber), "42")
	assertEqual(t, "HeadBranch", wp.HeadBranch, "feature-branch")
	assertEqual(t, "BaseBranch", wp.BaseBranch, "main")
	assertEqual(t, "Commit", wp.Commit, "headsha123")
}

// =============================================================================
// Azure DevOps Provider Tests (no external API calls)
// =============================================================================

func TestAzureDevOpsProvider_BuildCloneURL(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	tests := []struct {
		name        string
		accountName string
		token       string
		repoPath    string
		want        string
	}{
		{
			"project/repo with token",
			"myorg",
			"oauth2-token",
			"MyProject/MyRepo",
			"https://oauth2:oauth2-token@dev.azure.com/myorg/MyProject/_git/MyRepo",
		},
		{
			"project/repo without token",
			"myorg",
			"",
			"MyProject/MyRepo",
			"https://dev.azure.com/myorg/MyProject/_git/MyRepo",
		},
		{
			"single-segment path with token (fallback)",
			"myorg",
			"token",
			"MyRepo",
			"https://oauth2:token@dev.azure.com/myorg/MyRepo",
		},
		{
			"single-segment path without token (fallback)",
			"myorg",
			"",
			"MyRepo",
			"https://dev.azure.com/myorg/MyRepo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &models.VCSConnection{AccountName: tt.accountName}
			got := provider.BuildCloneURL(conn, tt.token, tt.repoPath)
			if got != tt.want {
				t.Errorf("BuildCloneURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAzureDevOpsProvider_ValidateWebhook(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	t.Run("empty secret passes", func(t *testing.T) {
		if err := provider.ValidateWebhook([]byte("payload"), "anything", ""); err != nil {
			t.Fatalf("expected nil error for empty secret, got: %v", err)
		}
	})

	t.Run("valid HMAC-SHA1 signature passes", func(t *testing.T) {
		secret := "my-ado-secret"
		payload := []byte(`{"eventType":"git.push"}`)
		mac := hmac.New(sha1.New, []byte(secret)) //nolint:gosec // G401: testing ADO signature
		mac.Write(payload)
		sig := "sha1=" + hex.EncodeToString(mac.Sum(nil))

		if err := provider.ValidateWebhook(payload, sig, secret); err != nil {
			t.Fatalf("expected valid signature to pass, got: %v", err)
		}
	})

	t.Run("invalid signature fails", func(t *testing.T) {
		if err := provider.ValidateWebhook([]byte("payload"), "sha1=bad", "secret"); err == nil {
			t.Fatal("expected error for invalid signature")
		}
	})
}

func TestAzureDevOpsProvider_ParseWebhookPayload_Push(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	payload := []byte(`{
		"eventType": "git.push",
		"resource": {
			"refUpdates": [
				{"name": "refs/heads/main", "newObjectId": "abc123def456789"}
			],
			"commits": [
				{
					"commitId": "abc123def456789",
					"author": {"name": "Dev User", "email": "dev@example.com"},
					"changes": [
						{"item": {"path": "/modules/networking/main.tf"}},
						{"item": {"path": "/modules/networking/variables.tf"}}
					]
				}
			],
			"repository": {
				"name": "infra-modules",
				"project": {"name": "CloudPlatform"}
			}
		}
	}`)

	wp, err := provider.ParseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertEqual(t, "EventType", wp.EventType, "push")
	assertEqual(t, "Repository", wp.Repository, "CloudPlatform/infra-modules")
	assertEqual(t, "Branch", wp.Branch, "main")
	assertEqual(t, "Commit", wp.Commit, "abc123def456789")
	assertEqual(t, "Committer", wp.Committer, "Dev User <dev@example.com>")

	if len(wp.ChangedFiles) != 2 {
		t.Fatalf("expected 2 changed files, got %d: %v", len(wp.ChangedFiles), wp.ChangedFiles)
	}
	if wp.ChangedFiles[0] != "/modules/networking/main.tf" {
		t.Errorf("ChangedFiles[0] = %q, want %q", wp.ChangedFiles[0], "/modules/networking/main.tf")
	}
}

func TestAzureDevOpsProvider_ParseWebhookPayload_PullRequest(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	payload := []byte(`{
		"eventType": "git.pullrequest.created",
		"resource": {
			"pullRequestId": 17,
			"status": "active",
			"sourceRefName": "refs/heads/feature/add-vpc",
			"targetRefName": "refs/heads/main",
			"lastMergeSourceCommit": {"commitId": "prhead123abc"},
			"createdBy": {
				"displayName": "Jane Doe",
				"uniqueName": "jane@example.com"
			},
			"repository": {
				"name": "infra-modules",
				"project": {"name": "CloudPlatform"}
			}
		}
	}`)

	wp, err := provider.ParseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertEqual(t, "EventType", wp.EventType, "pull_request")
	assertEqual(t, "Repository", wp.Repository, "CloudPlatform/infra-modules")
	assertEqual(t, "PRNumber", itoa(wp.PRNumber), "17")
	assertEqual(t, "HeadBranch", wp.HeadBranch, "feature/add-vpc")
	assertEqual(t, "BaseBranch", wp.BaseBranch, "main")
	assertEqual(t, "Commit", wp.Commit, "prhead123abc")
	assertEqual(t, "Committer", wp.Committer, "Jane Doe <jane@example.com>")
}

func TestAzureDevOpsProvider_ParseWebhookPayload_PullRequestCompleted(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	// Azure DevOps sends git.pullrequest.updated when a PR is completed.
	// Completed PRs should NOT be normalized to "pull_request" — they're merges, not open PRs.
	payload := []byte(`{
		"eventType": "git.pullrequest.updated",
		"resource": {
			"pullRequestId": 17,
			"status": "completed",
			"sourceRefName": "refs/heads/feature/add-vpc",
			"targetRefName": "refs/heads/main",
			"lastMergeSourceCommit": {"commitId": "prhead123abc"},
			"createdBy": {"displayName": "Jane", "uniqueName": "jane@example.com"},
			"repository": {"name": "repo", "project": {"name": "proj"}}
		}
	}`)

	wp, err := provider.ParseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Completed PRs get passed through with original eventType (NOT "pull_request")
	if wp.EventType == "pull_request" {
		t.Errorf("completed PR should not be normalized to pull_request, got EventType=%q", wp.EventType)
	}
	assertEqual(t, "EventType", wp.EventType, "git.pullrequest.updated")
}

func TestAzureDevOpsProvider_ParseWebhookPayload_UnknownEvent(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	payload := []byte(`{"eventType": "build.complete", "resource": {}}`)

	wp, err := provider.ParseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertEqual(t, "EventType", wp.EventType, "build.complete")
}

func TestAzureDevOpsProvider_ParseWebhookPayload_TagPush(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	// Azure DevOps sends tag pushes as git.push with refs/tags/ prefix
	payload := []byte(`{
		"eventType": "git.push",
		"resource": {
			"refUpdates": [
				{"name": "refs/tags/v1.2.0", "newObjectId": "tagsha456"}
			],
			"commits": [],
			"repository": {
				"name": "my-module",
				"project": {"name": "Infra"}
			}
		}
	}`)

	wp, err := provider.ParseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertEqual(t, "EventType", wp.EventType, "push")
	// TrimPrefix("refs/tags/v1.2.0", "refs/heads/") leaves it unchanged — tag pushes keep full ref
	assertEqual(t, "Branch", wp.Branch, "refs/tags/v1.2.0")
	assertEqual(t, "Repository", wp.Repository, "Infra/my-module")
	assertEqual(t, "Commit", wp.Commit, "tagsha456")
}

func TestAzureDevOpsProvider_ParseWebhookPayload_MultipleCommits(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	payload := []byte(`{
		"eventType": "git.push",
		"resource": {
			"refUpdates": [
				{"name": "refs/heads/develop", "newObjectId": "latest-sha"}
			],
			"commits": [
				{
					"commitId": "first-sha",
					"author": {"name": "Alice", "email": "alice@example.com"},
					"changes": [{"item": {"path": "/a.tf"}}]
				},
				{
					"commitId": "latest-sha",
					"author": {"name": "Bob", "email": "bob@example.com"},
					"changes": [{"item": {"path": "/b.tf"}}, {"item": {"path": "/c.tf"}}]
				}
			],
			"repository": {"name": "repo", "project": {"name": "proj"}}
		}
	}`)

	wp, err := provider.ParseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Commit should come from refUpdates, not the first commit
	assertEqual(t, "Commit", wp.Commit, "latest-sha")
	// Committer should be from the first commit
	assertEqual(t, "Committer", wp.Committer, "Alice <alice@example.com>")
	// All changed files across all commits should be collected
	if len(wp.ChangedFiles) != 3 {
		t.Fatalf("expected 3 changed files, got %d: %v", len(wp.ChangedFiles), wp.ChangedFiles)
	}
}

func TestAzureDevOpsProvider_ParseWebhookPayload_InvalidJSON(t *testing.T) {
	provider := vcs.NewAzureDevOpsProvider(nil)

	_, err := provider.ParseWebhookPayload([]byte(`{invalid json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestGitHubProvider_ParseWebhookPayload_InvalidJSON(t *testing.T) {
	provider := &vcs.GitHubProvider{}

	_, err := provider.ParseWebhookPayload([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// =============================================================================
// Cross-provider normalization tests
// =============================================================================

func TestWebhookPayload_Normalization(t *testing.T) {
	// Verify that both providers produce identical normalized payload structures
	// for equivalent events. This ensures handler code works provider-agnostically.

	ghProvider := &vcs.GitHubProvider{}
	adoProvider := vcs.NewAzureDevOpsProvider(nil)

	ghPayload := []byte(`{
		"ref": "refs/heads/main",
		"after": "abc123",
		"repository": {"full_name": "owner/repo"},
		"commits": [{"id":"abc123","added":["file.tf"],"modified":[],"removed":[],"author":{"name":"User","email":"user@example.com"}}]
	}`)

	adoPayload := []byte(`{
		"eventType": "git.push",
		"resource": {
			"refUpdates": [{"name": "refs/heads/main", "newObjectId": "abc123"}],
			"commits": [{"commitId":"abc123","author":{"name":"User","email":"user@example.com"},"changes":[{"item":{"path":"file.tf"}}]}],
			"repository": {"name": "repo", "project": {"name": "owner"}}
		}
	}`)

	ghWP, err := ghProvider.ParseWebhookPayload(ghPayload)
	if err != nil {
		t.Fatalf("GitHub parse error: %v", err)
	}

	adoWP, err := adoProvider.ParseWebhookPayload(adoPayload)
	if err != nil {
		t.Fatalf("ADO parse error: %v", err)
	}

	// Both should normalize to the same structure
	assertEqual(t, "EventType", ghWP.EventType, adoWP.EventType)
	assertEqual(t, "Branch", ghWP.Branch, adoWP.Branch)
	assertEqual(t, "Commit", ghWP.Commit, adoWP.Commit)
	assertEqual(t, "Repository", ghWP.Repository, adoWP.Repository)

	if len(ghWP.ChangedFiles) != len(adoWP.ChangedFiles) {
		t.Errorf("Changed files count mismatch: GitHub=%d, ADO=%d", len(ghWP.ChangedFiles), len(adoWP.ChangedFiles))
	}
}

// =============================================================================
// AzureDevOpsManager tests (no external API calls)
// =============================================================================

func TestAzureDevOpsManager_Disabled(t *testing.T) {
	// Without env vars, manager should be disabled but not error
	// Clear env vars to ensure clean state
	t.Setenv("AZURE_DEVOPS_CLIENT_ID", "")
	t.Setenv("AZURE_DEVOPS_CLIENT_SECRET", "")

	mgr, err := vcs.NewAzureDevOpsManager()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.IsEnabled() {
		t.Fatal("expected manager to be disabled without env vars")
	}
}

func TestAzureDevOpsManager_Enabled(t *testing.T) {
	t.Setenv("AZURE_DEVOPS_CLIENT_ID", "test-client-id")
	t.Setenv("AZURE_DEVOPS_CLIENT_SECRET", "test-secret")
	t.Setenv("AZURE_DEVOPS_REDIRECT_URI", "http://localhost/callback")

	mgr, err := vcs.NewAzureDevOpsManager()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mgr.IsEnabled() {
		t.Fatal("expected manager to be enabled with env vars set")
	}
	if mgr.GetClientID() != "test-client-id" {
		t.Errorf("GetClientID() = %q, want %q", mgr.GetClientID(), "test-client-id")
	}
}

func TestAzureDevOpsManager_AuthorizationURL(t *testing.T) {
	t.Setenv("AZURE_DEVOPS_CLIENT_ID", "my-app-id")
	t.Setenv("AZURE_DEVOPS_CLIENT_SECRET", "secret")
	t.Setenv("AZURE_DEVOPS_REDIRECT_URI", "http://localhost:5173/callback")

	mgr, err := vcs.NewAzureDevOpsManager()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	url := mgr.GetAuthorizationURL("myorg|/settings|uuid-abc")
	if !contains(url, "https://login.microsoftonline.com/common/oauth2/v2.0/authorize") {
		t.Errorf("URL should use Entra ID authorize endpoint, got: %s", url)
	}
	if !contains(url, "client_id=my-app-id") {
		t.Errorf("URL should contain client_id, got: %s", url)
	}
	if !contains(url, "response_type=code") {
		t.Errorf("URL should contain response_type=code, got: %s", url)
	}
	if !contains(url, "redirect_uri=") {
		t.Errorf("URL should contain redirect_uri, got: %s", url)
	}
	if !contains(url, "499b84ac-1321-427f-aa17-267ca6975798") {
		t.Errorf("URL should contain ADO resource ID in scope, got: %s", url)
	}
}

// =============================================================================
// Helpers
// =============================================================================

func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSub(s, substr))
}

func containsSub(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}

// =============================================================================
// Azure DevOps Status Service Tests
// =============================================================================

func TestMapStatusToADO(t *testing.T) {
	tests := []struct {
		name     string
		input    vcs.StatusState
		expected vcs.AzureDevOpsStatusState
	}{
		{"pending maps to pending", vcs.StatusStatePending, vcs.ADOStatusPending},
		{"success maps to succeeded", vcs.StatusStateSuccess, vcs.ADOStatusSucceeded},
		{"failure maps to failed", vcs.StatusStateFailure, vcs.ADOStatusFailed},
		{"error maps to error", vcs.StatusStateError, vcs.ADOStatusError},
		{"unknown maps to notSet", vcs.StatusState("unknown"), vcs.ADOStatusNotSet},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := vcs.MapStatusToADO(tt.input)
			if result != tt.expected {
				t.Errorf("MapStatusToADO(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestAzureDevOpsStatusService_New(t *testing.T) {
	// Test with nil manager — should create service without panic
	svc := vcs.NewAzureDevOpsStatusService(nil, nil)
	if svc == nil {
		t.Fatal("NewAzureDevOpsStatusService returned nil")
	}
}

func TestGetConnUpdater(t *testing.T) {
	// Test that registry returns the connector updater
	called := false
	updater := func(conn *models.VCSConnection) error {
		called = true
		return nil
	}
	registry := vcs.NewProviderRegistry(nil, nil, updater)
	got := registry.GetConnUpdater()
	if got == nil {
		t.Fatal("GetConnUpdater returned nil")
	}
	// Call it to verify it's the same function
	_ = got(&models.VCSConnection{})
	if !called {
		t.Error("GetConnUpdater returned a different function")
	}
}

func TestGetConnUpdater_Nil(t *testing.T) {
	registry := vcs.NewProviderRegistry(nil, nil, nil)
	got := registry.GetConnUpdater()
	if got != nil {
		t.Error("GetConnUpdater should return nil when no updater is set")
	}
}
