// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

type UserRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) Create(user *models.User) error {
	return r.db.Create(user).Error
}

func (r *UserRepository) GetByID(id uuid.UUID) (*models.User, error) {
	var user models.User
	err := r.db.First(&user, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *UserRepository) GetByEmail(email string) (*models.User, error) {
	var user models.User
	err := r.db.First(&user, "email = ?", email).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetByEmailCaseInsensitive retrieves a user by email (case-insensitive)
func (r *UserRepository) GetByEmailCaseInsensitive(email string) (*models.User, error) {
	var user models.User
	err := r.db.Where("LOWER(email) = LOWER(?)", email).First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *UserRepository) Update(user *models.User) error {
	return r.db.Save(user).Error
}

func (r *UserRepository) Upsert(user *models.User) error {
	return r.db.Where("id = ?", user.ID).Assign(*user).FirstOrCreate(user).Error
}

// GetOrCreateByZitadelSubject gets an existing user by Zitadel subject or creates a new one
// If user doesn't exist by subject, tries to find by email and update it (fixes admin user issue)
// Priority: Check by email FIRST if provided, then by ZitadelSubject (handles placeholder users)
func (r *UserRepository) GetOrCreateByZitadelSubject(subject string, email, name string) (*models.User, error) {
	var user models.User

	// PRIORITY 1: Check by email FIRST if provided (handles placeholder users with "invited-" prefix)
	// This ensures we always find and update placeholder users
	if email != "" {
		emailErr := r.db.Where("LOWER(email) = LOWER(?)", email).First(&user).Error
		if emailErr == nil {
			// Found user by email, update subject if it's a placeholder (starts with "invited-") or empty
			oldSubject := user.ZitadelSubject
			logger.Debugf("GetOrCreateByZitadelSubject - Found user by email %s: ID=%s, ZitadelSubject=%s, newSubject=%s", email, user.ID, oldSubject, subject)
			shouldUpdate := false
			if user.ZitadelSubject == "" || strings.HasPrefix(user.ZitadelSubject, "invited-") {
				// Before updating, check if there's already a user with the target ZitadelSubject
				var existingUser models.User
				duplicateErr := r.db.Where("zitadel_subject = ?", subject).First(&existingUser).Error
				if duplicateErr == nil {
					// Duplicate user exists - need to handle it
					logger.Debugf("GetOrCreateByZitadelSubject - Duplicate user found with ZitadelSubject=%s: ID=%s, email=%s", subject, existingUser.ID, existingUser.Email)
					if existingUser.ID != user.ID {
						// Only delete if it's a different user (not the same user)
						// If duplicate user has no email, it's likely an empty duplicate created by mistake
						if existingUser.Email == "" {
							logger.Debugf("GetOrCreateByZitadelSubject - Deleting duplicate user %s (empty email)", existingUser.ID)
							if err := r.db.Delete(&existingUser).Error; err != nil {
								logger.Errorf("GetOrCreateByZitadelSubject - Failed to delete duplicate user %s: %v", existingUser.ID, err)
								return nil, fmt.Errorf("failed to delete duplicate user %s: %w", existingUser.ID, err)
							}
							logger.Debugf("GetOrCreateByZitadelSubject - Successfully deleted duplicate user %s", existingUser.ID)
						} else {
							// Duplicate user has an email - this is a conflict, return error
							logger.Errorf("GetOrCreateByZitadelSubject - Duplicate user %s with ZitadelSubject=%s already has email %s", existingUser.ID, subject, existingUser.Email)
							return nil, fmt.Errorf("duplicate user conflict: user %s with email %s already has ZitadelSubject=%s", existingUser.ID, existingUser.Email, subject)
						}
					}
				} else if duplicateErr != gorm.ErrRecordNotFound {
					// Error other than "not found"
					logger.Errorf("GetOrCreateByZitadelSubject - Error checking for duplicate user: %v", duplicateErr)
					return nil, fmt.Errorf("error checking for duplicate user: %w", duplicateErr)
				}
				// No duplicate or duplicate deleted, proceed with update
				logger.Debugf("GetOrCreateByZitadelSubject - Updating ZitadelSubject from %s to %s", oldSubject, subject)
				user.ZitadelSubject = subject
				shouldUpdate = true
			} else {
				logger.Debugf("GetOrCreateByZitadelSubject - User already has ZitadelSubject=%s, not updating", oldSubject)
			}
			if email != "" && user.Email != email {
				user.Email = email
				shouldUpdate = true
			}
			if name != "" && user.Name != name {
				user.Name = name
				shouldUpdate = true
			}
			// Save if something changed
			if shouldUpdate {
				logger.Debugf("GetOrCreateByZitadelSubject - Saving user %s with ZitadelSubject=%s", user.ID, user.ZitadelSubject)
				if err := r.db.Save(&user).Error; err != nil {
					logger.Errorf("GetOrCreateByZitadelSubject - Failed to update user %s: %v", user.ID, err)
					return nil, fmt.Errorf("failed to update user %s (old subject: %s, new subject: %s): %w", user.ID, oldSubject, subject, err)
				}
				logger.Debugf("GetOrCreateByZitadelSubject - Successfully updated user %s", user.ID)
			} else {
				logger.Debugf("GetOrCreateByZitadelSubject - No updates needed for user %s", user.ID)
			}
			return &user, nil
		}
		logger.Debugf("GetOrCreateByZitadelSubject - User not found by email %s: %v", email, emailErr)
		// If not found by email and error is not "not found", return the error
		if emailErr != gorm.ErrRecordNotFound {
			return nil, emailErr
		}
	}

	// PRIORITY 2: Try to find existing user by Zitadel subject
	err := r.db.Where("zitadel_subject = ?", subject).First(&user).Error
	if err == nil {
		// User exists, optionally update email/name if provided
		if email != "" && user.Email != email {
			user.Email = email
		}
		if name != "" && user.Name != name {
			user.Name = name
		}
		if email != "" || name != "" {
			if err := r.db.Save(&user).Error; err != nil {
				return nil, err
			}
		}
		return &user, nil
	}

	// PRIORITY 3: User doesn't exist by subject or email
	// Before creating, check if there's a placeholder user with empty email that we can update
	// This handles the case where email is empty in the JWT token but a placeholder user exists
	if err == gorm.ErrRecordNotFound {
		// Try to find placeholder user with empty email and "invited-" prefix
		placeholderErr := r.db.Where("email = '' OR email IS NULL").Where("zitadel_subject LIKE ?", "invited-%").First(&user).Error
		if placeholderErr == nil {
			// Found placeholder user with empty email, update it
			oldSubject := user.ZitadelSubject
			logger.Debugf("GetOrCreateByZitadelSubject - Found placeholder user with empty email: ID=%s, ZitadelSubject=%s, newSubject=%s", user.ID, oldSubject, subject)
			user.ZitadelSubject = subject
			shouldUpdate := true
			if email != "" && user.Email != email {
				user.Email = email
				shouldUpdate = true
			}
			if name != "" && user.Name != name {
				user.Name = name
				shouldUpdate = true
			}
			if shouldUpdate {
				logger.Debugf("GetOrCreateByZitadelSubject - Updating placeholder user %s with ZitadelSubject=%s", user.ID, user.ZitadelSubject)
				if err := r.db.Save(&user).Error; err != nil {
					logger.Errorf("GetOrCreateByZitadelSubject - Failed to update placeholder user %s: %v", user.ID, err)
					return nil, fmt.Errorf("failed to update placeholder user %s (old subject: %s, new subject: %s): %w", user.ID, oldSubject, subject, err)
				}
				logger.Debugf("GetOrCreateByZitadelSubject - Successfully updated placeholder user %s", user.ID)
			}
			return &user, nil
		}
		// No placeholder user found, proceed to create new user
		logger.Debugf("GetOrCreateByZitadelSubject - No placeholder user found, creating new user with subject=%s, email=%s", subject, email)
	}

	// Create new user
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	user = models.User{
		ZitadelSubject: subject,
		Email:          email,
		Name:           name,
	}

	if err := r.db.Create(&user).Error; err != nil {
		// If creation fails due to unique constraint on email, try to find existing user with empty email
		if strings.Contains(err.Error(), "idx_users_email") && email == "" {
			logger.Debugf("GetOrCreateByZitadelSubject - User creation failed due to email constraint, looking for existing user with empty email")
			findErr := r.db.Where("(email = '' OR email IS NULL)").First(&user).Error
			if findErr == nil {
				// Found existing user with empty email, update it
				oldSubject := user.ZitadelSubject
				if user.ZitadelSubject == "" || strings.HasPrefix(user.ZitadelSubject, "invited-") {
					logger.Debugf("GetOrCreateByZitadelSubject - Found existing user with empty email, updating ZitadelSubject from %s to %s", oldSubject, subject)
					user.ZitadelSubject = subject
					if name != "" && user.Name != name {
						user.Name = name
					}
					if err := r.db.Save(&user).Error; err != nil {
						return nil, fmt.Errorf("failed to update user %s: %w", user.ID, err)
					}
					return &user, nil
				}
			}
		}
		return nil, err
	}

	return &user, nil
}
