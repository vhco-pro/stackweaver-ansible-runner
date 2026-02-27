// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package sessions

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/zitadel/zitadel-go/v3/pkg/client"
	v2 "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/object/v2"
	sessionV2 "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/session/v2"
	zitadelpkg "github.com/zitadel/zitadel-go/v3/pkg/zitadel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Service handles session operations using Zitadel's gRPC API
type Service struct {
	sessionService sessionV2.SessionServiceClient
	ctx            context.Context
}

// NewService creates a new sessions service
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
		sessionService: api.SessionServiceV2(),
		ctx:            ctx,
	}, nil
}

// ListUserSessions lists all active sessions for a user
// According to Zitadel Session Service V2 API: https://zitadel.com/docs/apis/resources/session_service_v2/session-service-list-sessions
// This uses the ListSessions gRPC method with a UserIdQuery to filter sessions by user ID
func (s *Service) ListUserSessions(userID string) ([]*Session, error) {
	if userID == "" {
		return nil, fmt.Errorf("user ID is required")
	}

	// Search for sessions by user ID
	// According to Zitadel API documentation, we use ListSessions with Queries containing UserIdQuery
	// This is the gRPC equivalent of POST /v2/sessions/search with a user ID filter
	req := &sessionV2.ListSessionsRequest{
		Queries: []*sessionV2.SearchQuery{
			{
				Query: &sessionV2.SearchQuery_UserIdQuery{
					UserIdQuery: &sessionV2.UserIDQuery{
						Id: userID,
					},
				},
			},
		},
		Query: &v2.ListQuery{
			Limit: 100, // Get up to 100 sessions
		},
		SortingColumn: sessionV2.SessionFieldName_SESSION_FIELD_NAME_CREATION_DATE,
	}

	resp, err := s.sessionService.ListSessions(s.ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions for user %s: %w", userID, err)
	}

	sessions := []*Session{}
	for _, session := range resp.GetSessions() {
		var expirationDate time.Time
		if exp := session.GetExpirationDate(); exp != nil {
			expirationDate = exp.AsTime()
		}

		// Only include sessions that haven't expired (if expiration is set)
		now := time.Now()
		if !expirationDate.IsZero() && expirationDate.Before(now) {
			continue // Skip expired sessions
		}

		// Build a more descriptive user agent string
		userAgentStr := buildUserAgentString(session)

		// Extract IP address for matching
		var sessionIP string
		if ua := session.GetUserAgent(); ua != nil {
			sessionIP = ua.GetIp()
		}

		sessions = append(sessions, &Session{
			ID:             session.GetId(),
			UserID:         userID, // We know the user ID from the query
			UserAgent:      userAgentStr,
			IPAddress:      sessionIP,
			CreationDate:   session.GetCreationDate().AsTime(),
			ExpirationDate: expirationDate,
			Factors:        extractFactors(session.GetFactors()),
		})
	}

	return sessions, nil
}

// RevokeSession revokes a session by ID
// According to Zitadel Session Service V2 API: https://zitadel.com/docs/apis/resources/session_service_v2/session-service-delete-session
// This uses the DeleteSession gRPC method (equivalent to DELETE /v2/sessions/{sessionId})
// SessionToken is optional when using a PAT with session.delete permission (which we have via login service PAT)
func (s *Service) RevokeSession(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session ID is required")
	}

	_, err := s.sessionService.DeleteSession(s.ctx, &sessionV2.DeleteSessionRequest{
		SessionId: sessionID,
		// SessionToken is optional when:
		// - the caller has session.delete permission (we have this via login service PAT)
		// - the authenticated user requests their own session
		// - the security token has the same user agent as the session
		// Since we're using a PAT with proper permissions, we don't need to provide SessionToken
	})
	if err != nil {
		return fmt.Errorf("failed to revoke session %s: %w", sessionID, err)
	}

	return nil
}

// buildUserAgentString builds a descriptive string for the session device
func buildUserAgentString(session *sessionV2.Session) string {
	parts := []string{}

	if ua := session.GetUserAgent(); ua != nil {
		// Try description first (most user-friendly)
		if desc := ua.GetDescription(); desc != "" {
			return desc
		}

		// Try to build from available fields
		if ip := ua.GetIp(); ip != "" {
			parts = append(parts, fmt.Sprintf("IP: %s", ip))
		}
		if fp := ua.GetFingerprintId(); fp != "" {
			parts = append(parts, fmt.Sprintf("Device ID: %s", fp))
		}
	}

	// If we have some info, combine it
	if len(parts) > 0 {
		return fmt.Sprintf("Session (%s)", parts[0])
	}

	// Fallback: use session ID (shortened) and creation date
	creationDate := session.GetCreationDate().AsTime()
	sessionID := session.GetId()
	if len(sessionID) > 8 {
		sessionID = sessionID[:8] + "..."
	}
	return fmt.Sprintf("Session %s (created %s)", sessionID, creationDate.Format("Jan 2, 2006"))
}

// extractFactors extracts authentication factors from session factors
func extractFactors(factors *sessionV2.Factors) []string {
	factorList := []string{}
	if factors == nil {
		return factorList
	}

	if factors.GetUser() != nil {
		factorList = append(factorList, "User")
	}
	if factors.GetPassword() != nil {
		factorList = append(factorList, "Password")
	}
	if factors.GetWebAuthN() != nil {
		factorList = append(factorList, "WebAuthN")
	}
	if factors.GetTotp() != nil {
		factorList = append(factorList, "TOTP")
	}
	if factors.GetOtpSms() != nil {
		factorList = append(factorList, "OTP SMS")
	}
	if factors.GetOtpEmail() != nil {
		factorList = append(factorList, "OTP Email")
	}
	if factors.GetIntent() != nil {
		factorList = append(factorList, "Intent")
	}

	return factorList
}

// Session represents a user session
type Session struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	UserAgent      string    `json:"user_agent"`
	IPAddress      string    `json:"ip_address,omitempty"`
	CreationDate   time.Time `json:"creation_date"`
	ExpirationDate time.Time `json:"expiration_date"`
	Factors        []string  `json:"factors"`
}
