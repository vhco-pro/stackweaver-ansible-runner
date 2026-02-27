// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package totp

import (
	"context"
	"fmt"
	"net"

	"github.com/zitadel/zitadel-go/v3/pkg/client"
	userV2 "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/user/v2"
	zitadelpkg "github.com/zitadel/zitadel-go/v3/pkg/zitadel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Service handles TOTP operations using Zitadel's gRPC API
type Service struct {
	userService userV2.UserServiceClient
	ctx         context.Context
}

// NewService creates a new TOTP service
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

// StartTOTPRegistration starts TOTP registration for a user
// Returns the secret and URL for QR code generation
func (s *Service) StartTOTPRegistration(userID string) (*TOTPRegistrationResponse, error) {
	resp, err := s.userService.RegisterTOTP(s.ctx, &userV2.RegisterTOTPRequest{
		UserId: userID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start TOTP registration: %w", err)
	}

	return &TOTPRegistrationResponse{
		Secret: resp.GetSecret(),
		URL:    resp.GetUri(),
	}, nil
}

// VerifyTOTP verifies a TOTP code for a user
func (s *Service) VerifyTOTP(userID, code string) error {
	_, err := s.userService.VerifyTOTPRegistration(s.ctx, &userV2.VerifyTOTPRegistrationRequest{
		UserId: userID,
		Code:   code,
	})
	if err != nil {
		return fmt.Errorf("failed to verify TOTP: %w", err)
	}

	return nil
}

// RemoveTOTP removes TOTP from a user
func (s *Service) RemoveTOTP(userID string) error {
	_, err := s.userService.RemoveTOTP(s.ctx, &userV2.RemoveTOTPRequest{
		UserId: userID,
	})
	if err != nil {
		return fmt.Errorf("failed to remove TOTP: %w", err)
	}

	return nil
}

// CheckTOTPStatus checks if a user has TOTP enabled
func (s *Service) CheckTOTPStatus(userID string) (bool, error) {
	// List authentication factors to check if TOTP is enabled
	resp, err := s.userService.ListAuthenticationFactors(s.ctx, &userV2.ListAuthenticationFactorsRequest{
		UserId: userID,
	})
	if err != nil {
		return false, fmt.Errorf("failed to list authentication factors: %w", err)
	}

	// Check if TOTP is in the list of factors
	// TOTP is represented by the Otp field in AuthFactor
	for _, factor := range resp.GetResult() {
		if factor.GetOtp() != nil {
			return true, nil
		}
	}

	return false, nil
}

// ChangePassword changes a user's password
func (s *Service) ChangePassword(userID, currentPassword, newPassword string) error {
	_, err := s.userService.UpdateUser(s.ctx, &userV2.UpdateUserRequest{
		UserId: userID,
		UserType: &userV2.UpdateUserRequest_Human_{
			Human: &userV2.UpdateUserRequest_Human{
				Password: &userV2.SetPassword{
					PasswordType: &userV2.SetPassword_Password{
						Password: &userV2.Password{
							Password: newPassword,
						},
					},
					Verification: &userV2.SetPassword_CurrentPassword{
						CurrentPassword: currentPassword,
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to change password: %w", err)
	}

	return nil
}

// ListMFADevices lists all MFA devices for a user
func (s *Service) ListMFADevices(userID string) ([]*MFADevice, error) {
	devices := []*MFADevice{}

	// List authentication factors (TOTP, U2F, OTP SMS, OTP Email)
	factorsResp, err := s.userService.ListAuthenticationFactors(s.ctx, &userV2.ListAuthenticationFactorsRequest{
		UserId: userID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list authentication factors: %w", err)
	}

	for _, factor := range factorsResp.GetResult() {
		var deviceType string
		var deviceName string

		switch {
		case factor.GetOtp() != nil:
			deviceType = "TOTP"
			deviceName = "Authenticator App"
		case factor.GetU2F() != nil:
			deviceType = "U2F"
			deviceName = "Security Key (U2F)"
		case factor.GetOtpSms() != nil:
			deviceType = "OTP_SMS"
			deviceName = "SMS One-Time Password"
		case factor.GetOtpEmail() != nil:
			deviceType = "OTP_EMAIL"
			deviceName = "Email One-Time Password"
		default:
			continue
		}

		devices = append(devices, &MFADevice{
			Type:  deviceType,
			Name:  deviceName,
			State: factor.GetState().String(),
		})
	}

	// List passkeys
	passkeysResp, err := s.userService.ListPasskeys(s.ctx, &userV2.ListPasskeysRequest{
		UserId: userID,
	})
	if err == nil {
		for _, passkey := range passkeysResp.GetResult() {
			name := passkey.GetName()
			if name == "" {
				name = "Passkey"
			}
			devices = append(devices, &MFADevice{
				Type:  "PASSKEY",
				Name:  name,
				ID:    passkey.GetId(),
				State: passkey.GetState().String(),
			})
		}
	}

	return devices, nil
}

// TOTPRegistrationResponse contains the TOTP secret and URL for QR code
type TOTPRegistrationResponse struct {
	Secret string `json:"secret"` //nolint:gosec // G117: TOTP secret field for QR code registration
	URL    string `json:"url"`
}

// MFADevice represents an MFA device
type MFADevice struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	ID    string `json:"id,omitempty"`
	State string `json:"state"`
}
