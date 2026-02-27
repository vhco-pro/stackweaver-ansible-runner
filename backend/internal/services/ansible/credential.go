// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
)

// CredentialService handles Ansible credential operations
type CredentialService struct {
	credentialRepo *repository.AnsibleCredentialRepository
	encryptionKey  []byte
}

// NewCredentialService creates a new credential service
func NewCredentialService(credentialRepo *repository.AnsibleCredentialRepository, encryptionKey []byte) *CredentialService {
	return &CredentialService{
		credentialRepo: credentialRepo,
		encryptionKey:  encryptionKey,
	}
}

// CreateCredentialInput represents the input for creating a credential
type CreateCredentialInput struct {
	OrganizationID     uuid.UUID
	ProjectID          *uuid.UUID
	Name               string
	Description        string
	Type               models.CredentialType
	Username           string
	SSHPrivateKey      string
	SSHPassphrase      string
	Password           string //nolint:gosec // G117: credential field
	VaultPassword      string
	BecomePassword     string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AzureTenantID      string
	AzureClientID      string
	AzureClientSecret  string
	GCPServiceAccount  string
	SSHPort            int
	SSHBecomeUser      string
}

// UpdateCredentialInput represents the input for updating a credential
type UpdateCredentialInput struct {
	ProjectID          *uuid.UUID
	Name               *string
	Description        *string
	Username           *string
	SSHPrivateKey      *string
	SSHPassphrase      *string
	Password           *string //nolint:gosec // G117: credential field
	VaultPassword      *string
	BecomePassword     *string
	AWSAccessKeyID     *string
	AWSSecretAccessKey *string
	AzureTenantID      *string
	AzureClientID      *string
	AzureClientSecret  *string
	GCPServiceAccount  *string
	SSHPort            *int
	SSHBecomeUser      *string
}

// CreateCredential creates a new credential with encrypted sensitive fields
func (s *CredentialService) CreateCredential(input CreateCredentialInput) (*models.AnsibleCredential, error) {
	credential := &models.AnsibleCredential{
		OrganizationID: input.OrganizationID,
		ProjectID:      input.ProjectID,
		Name:           input.Name,
		Description:    input.Description,
		Type:           input.Type,
		Username:       input.Username,
		AzureTenantID:  input.AzureTenantID,
		AzureClientID:  input.AzureClientID,
		SSHPort:        input.SSHPort,
		SSHBecomeUser:  input.SSHBecomeUser,
	}

	if credential.SSHPort == 0 {
		credential.SSHPort = 22
	}
	if credential.SSHBecomeUser == "" {
		credential.SSHBecomeUser = "root"
	}

	// Encrypt sensitive fields
	if input.SSHPrivateKey != "" {
		encrypted, err := s.encrypt(input.SSHPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt SSH private key: %w", err)
		}
		credential.SSHPrivateKey = encrypted
	}

	if input.SSHPassphrase != "" {
		encrypted, err := s.encrypt(input.SSHPassphrase)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt SSH passphrase: %w", err)
		}
		credential.SSHPassphrase = encrypted
	}

	if input.Password != "" {
		encrypted, err := s.encrypt(input.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt password: %w", err)
		}
		credential.Password = encrypted
	}

	if input.VaultPassword != "" {
		encrypted, err := s.encrypt(input.VaultPassword)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt vault password: %w", err)
		}
		credential.VaultPassword = encrypted
	}

	if input.BecomePassword != "" {
		encrypted, err := s.encrypt(input.BecomePassword)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt become password: %w", err)
		}
		credential.BecomePassword = encrypted
	}

	if input.AWSAccessKeyID != "" {
		encrypted, err := s.encrypt(input.AWSAccessKeyID)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt AWS access key ID: %w", err)
		}
		credential.AWSAccessKeyID = encrypted
	}

	if input.AWSSecretAccessKey != "" {
		encrypted, err := s.encrypt(input.AWSSecretAccessKey)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt AWS secret access key: %w", err)
		}
		credential.AWSSecretAccessKey = encrypted
	}

	if input.AzureClientSecret != "" {
		encrypted, err := s.encrypt(input.AzureClientSecret)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt Azure client secret: %w", err)
		}
		credential.AzureClientSecret = encrypted
	}

	if input.GCPServiceAccount != "" {
		encrypted, err := s.encrypt(input.GCPServiceAccount)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt GCP service account: %w", err)
		}
		credential.GCPServiceAccount = encrypted
	}

	if err := s.credentialRepo.Create(credential); err != nil {
		return nil, err
	}

	return credential, nil
}

// GetCredential retrieves a credential by ID (without decrypting sensitive fields)
func (s *CredentialService) GetCredential(id uuid.UUID) (*models.AnsibleCredential, error) {
	return s.credentialRepo.GetByID(id)
}

// GetDecryptedCredential retrieves a credential with decrypted sensitive fields
// This should only be used by the runner or internal services
func (s *CredentialService) GetDecryptedCredential(id uuid.UUID) (*models.AnsibleCredential, error) {
	credential, err := s.credentialRepo.GetByID(id)
	if err != nil {
		return nil, err
	}

	// Decrypt sensitive fields
	if credential.SSHPrivateKey != "" {
		decrypted, err := s.decrypt(credential.SSHPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt SSH private key: %w", err)
		}
		credential.SSHPrivateKey = decrypted
	}

	if credential.SSHPassphrase != "" {
		decrypted, err := s.decrypt(credential.SSHPassphrase)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt SSH passphrase: %w", err)
		}
		credential.SSHPassphrase = decrypted
	}

	if credential.Password != "" {
		decrypted, err := s.decrypt(credential.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt password: %w", err)
		}
		credential.Password = decrypted
	}

	if credential.VaultPassword != "" {
		decrypted, err := s.decrypt(credential.VaultPassword)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt vault password: %w", err)
		}
		credential.VaultPassword = decrypted
	}

	if credential.BecomePassword != "" {
		decrypted, err := s.decrypt(credential.BecomePassword)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt become password: %w", err)
		}
		credential.BecomePassword = decrypted
	}

	if credential.AWSAccessKeyID != "" {
		decrypted, err := s.decrypt(credential.AWSAccessKeyID)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt AWS access key ID: %w", err)
		}
		credential.AWSAccessKeyID = decrypted
	}

	if credential.AWSSecretAccessKey != "" {
		decrypted, err := s.decrypt(credential.AWSSecretAccessKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt AWS secret access key: %w", err)
		}
		credential.AWSSecretAccessKey = decrypted
	}

	if credential.AzureClientSecret != "" {
		decrypted, err := s.decrypt(credential.AzureClientSecret)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt Azure client secret: %w", err)
		}
		credential.AzureClientSecret = decrypted
	}

	if credential.GCPServiceAccount != "" {
		decrypted, err := s.decrypt(credential.GCPServiceAccount)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt GCP service account: %w", err)
		}
		credential.GCPServiceAccount = decrypted
	}

	return credential, nil
}

// ListCredentials lists all credentials for an organization
func (s *CredentialService) ListCredentials(orgID uuid.UUID, limit, offset int) ([]models.AnsibleCredential, int64, error) {
	return s.credentialRepo.ListByOrganization(orgID, limit, offset)
}

// ListCredentialsByType lists credentials by type for an organization
func (s *CredentialService) ListCredentialsByType(orgID uuid.UUID, credType models.CredentialType, limit, offset int) ([]models.AnsibleCredential, int64, error) {
	return s.credentialRepo.ListByOrganizationAndType(orgID, credType, limit, offset)
}

// UpdateCredential updates a credential
func (s *CredentialService) UpdateCredential(id uuid.UUID, input UpdateCredentialInput) (*models.AnsibleCredential, error) {
	credential, err := s.credentialRepo.GetByID(id)
	if err != nil {
		return nil, err
	}

	if input.ProjectID != nil {
		credential.ProjectID = input.ProjectID
	}
	if input.Name != nil {
		credential.Name = *input.Name
	}
	if input.Description != nil {
		credential.Description = *input.Description
	}
	if input.Username != nil {
		credential.Username = *input.Username
	}
	if input.AzureTenantID != nil {
		credential.AzureTenantID = *input.AzureTenantID
	}
	if input.AzureClientID != nil {
		credential.AzureClientID = *input.AzureClientID
	}
	if input.SSHPort != nil {
		credential.SSHPort = *input.SSHPort
	}
	if input.SSHBecomeUser != nil {
		credential.SSHBecomeUser = *input.SSHBecomeUser
	}

	// Re-encrypt sensitive fields if provided
	if input.SSHPrivateKey != nil && *input.SSHPrivateKey != "" {
		encrypted, err := s.encrypt(*input.SSHPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt SSH private key: %w", err)
		}
		credential.SSHPrivateKey = encrypted
	}

	if input.SSHPassphrase != nil && *input.SSHPassphrase != "" {
		encrypted, err := s.encrypt(*input.SSHPassphrase)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt SSH passphrase: %w", err)
		}
		credential.SSHPassphrase = encrypted
	}

	if input.Password != nil && *input.Password != "" {
		encrypted, err := s.encrypt(*input.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt password: %w", err)
		}
		credential.Password = encrypted
	}

	if input.VaultPassword != nil && *input.VaultPassword != "" {
		encrypted, err := s.encrypt(*input.VaultPassword)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt vault password: %w", err)
		}
		credential.VaultPassword = encrypted
	}

	if input.BecomePassword != nil && *input.BecomePassword != "" {
		encrypted, err := s.encrypt(*input.BecomePassword)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt become password: %w", err)
		}
		credential.BecomePassword = encrypted
	}

	if input.AWSAccessKeyID != nil && *input.AWSAccessKeyID != "" {
		encrypted, err := s.encrypt(*input.AWSAccessKeyID)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt AWS access key ID: %w", err)
		}
		credential.AWSAccessKeyID = encrypted
	}

	if input.AWSSecretAccessKey != nil && *input.AWSSecretAccessKey != "" {
		encrypted, err := s.encrypt(*input.AWSSecretAccessKey)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt AWS secret access key: %w", err)
		}
		credential.AWSSecretAccessKey = encrypted
	}

	if input.AzureClientSecret != nil && *input.AzureClientSecret != "" {
		encrypted, err := s.encrypt(*input.AzureClientSecret)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt Azure client secret: %w", err)
		}
		credential.AzureClientSecret = encrypted
	}

	if input.GCPServiceAccount != nil && *input.GCPServiceAccount != "" {
		encrypted, err := s.encrypt(*input.GCPServiceAccount)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt GCP service account: %w", err)
		}
		credential.GCPServiceAccount = encrypted
	}

	if err := s.credentialRepo.Update(credential); err != nil {
		return nil, err
	}

	return credential, nil
}

// DeleteCredential deletes a credential
func (s *CredentialService) DeleteCredential(id uuid.UUID) error {
	return s.credentialRepo.Delete(id)
}

// encrypt encrypts data using AES-256-GCM
func (s *CredentialService) encrypt(plaintext string) (string, error) {
	if len(s.encryptionKey) == 0 {
		return "", fmt.Errorf("encryption key not configured")
	}

	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decrypt decrypts data using AES-256-GCM
func (s *CredentialService) decrypt(ciphertext string) (string, error) {
	if len(s.encryptionKey) == 0 {
		return "", fmt.Errorf("encryption key not configured")
	}

	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, cipherData := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, cipherData, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
