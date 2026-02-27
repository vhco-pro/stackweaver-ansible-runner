// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package profile

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/zitadel/zitadel-go/v3/pkg/client"
	userV2 "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/user/v2"
	zitadelpkg "github.com/zitadel/zitadel-go/v3/pkg/zitadel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Service handles user profile operations using Zitadel's gRPC API
type Service struct {
	userService userV2.UserServiceClient
	ctx         context.Context
}

// NewService creates a new profile service
func NewService(zitadelIssuer, zitadelInternalAddr, loginServicePAT string) (*Service, error) {
	ctx := context.Background()

	// Use localhost as domain to match ExternalDomain
	zitadelInstance := zitadelpkg.New("localhost", zitadelpkg.WithInsecure("8080"))

	if zitadelInternalAddr == "" {
		zitadelInternalAddr = "internal-zitadel:8080"
	}

	// Create a custom dialer that connects to the Docker network alias
	customDialer := func(ctx context.Context, addr string) (net.Conn, error) {
		dialer := &net.Dialer{}
		return dialer.DialContext(ctx, "tcp", zitadelInternalAddr)
	}

	// Create client with login service PAT
	api, err := client.New(ctx, zitadelInstance,
		client.WithAuth(client.PAT(loginServicePAT)),
		client.WithGRPCDialOptions(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(customDialer),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Zitadel client: %w", err)
	}

	return &Service{
		userService: api.UserServiceV2(),
		ctx:         ctx,
	}, nil
}

// GetUserProfile gets user profile information from Zitadel
func (s *Service) GetUserProfile(userID string) (*UserProfile, error) {
	resp, err := s.userService.GetUserByID(s.ctx, &userV2.GetUserByIDRequest{
		UserId: userID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get user profile: %w", err)
	}

	user := resp.GetUser()
	human := user.GetHuman()

	profile := &UserProfile{
		ID:    userID,
		Email: "",
		Name:  "",
	}

	if human != nil {
		// Safely get email with nil check
		if email := human.GetEmail(); email != nil {
			profile.Email = email.GetEmail()
		}

		// Safely get profile information
		if human.GetProfile() != nil {
			givenName := human.GetProfile().GetGivenName()
			familyName := human.GetProfile().GetFamilyName()
			if givenName != "" || familyName != "" {
				profile.Name = strings.TrimSpace(givenName + " " + familyName)
			}
			if profile.Name == "" {
				profile.Name = human.GetProfile().GetDisplayName()
			}
			if profile.Name == "" {
				profile.Name = human.GetProfile().GetNickName()
			}
		}
	}

	return profile, nil
}

// UpdateUserProfile updates user profile information in Zitadel
// According to Zitadel API: https://zitadel.com/docs/apis/resources/user_service_v2/user-service-update-user
// This implements PATCH /v2/users/:userId endpoint
func (s *Service) UpdateUserProfile(userID string, profile *UpdateProfileRequest) error {
	updateReq := &userV2.UpdateUserRequest{
		UserId: userID,
		UserType: &userV2.UpdateUserRequest_Human_{
			Human: &userV2.UpdateUserRequest_Human{},
		},
	}

	// Update profile (name) - parse into given and family name
	// According to Zitadel API, profile update requires GivenName and FamilyName
	// Note: Zitadel doesn't allow empty names, so we skip update if name is empty
	if profile.Name != "" {
		parts := splitName(profile.Name)
		givenName := parts[0]
		familyName := ""
		if len(parts) > 1 {
			familyName = strings.Join(parts[1:], " ")
		}

		// Set profile with given and family name
		// Optional fields like NickName, DisplayName, PreferredLanguage, Gender can be added here if needed
		updateReq.GetHuman().Profile = &userV2.UpdateUserRequest_Human_Profile{
			GivenName:  &givenName,
			FamilyName: &familyName,
		}
	}

	// Update email if provided
	// According to Zitadel API, email updates can include verification settings
	// Note: Zitadel doesn't allow empty emails, so we skip update if email is empty
	// If verification is not set, Zitadel will handle it according to instance settings
	if profile.Email != "" {
		updateReq.GetHuman().Email = &userV2.SetHumanEmail{
			Email: profile.Email,
			// Verification can be set to:
			// - nil (default behavior - Zitadel handles verification automatically)
			// - SendCode (sends verification code via email)
			// - ReturnCode (returns verification code in response)
			// For now, we'll let Zitadel handle verification automatically
		}
	}

	_, err := s.userService.UpdateUser(s.ctx, updateReq)
	if err != nil {
		return fmt.Errorf("failed to update user profile: %w", err)
	}

	return nil
}

// splitName splits a full name into first and last name
func splitName(fullName string) []string {
	parts := []string{}
	current := ""
	for _, char := range fullName {
		if char == ' ' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		} else {
			current += string(char)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	if len(parts) == 0 {
		return []string{fullName}
	}
	return parts
}

// UserProfile represents user profile information
type UserProfile struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// UpdateProfileRequest represents a request to update user profile
type UpdateProfileRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}
