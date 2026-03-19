package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/k8shell-io/k8shelld/internal/models"
)

const identityRefreshInterval = 15 * time.Second

// loadIdentity reads the identity JWT from the path configured in
// cfg.Identity, verifies it using the configured public key and signing method,
// and populates cfg.User with the verified claims.
func (s *Server) loadIdentity() error {
	if s.testMode {
		return nil
	}

	tokenBytes, err := os.ReadFile(s.config.Identity.TokenPath)
	if err != nil {
		return fmt.Errorf("read identity token: %w", err)
	}
	tokenStr := strings.TrimSpace(string(tokenBytes))

	claims, err := s.jwtVerifier.VerifyToken(tokenStr)
	if err != nil {
		return fmt.Errorf("verify identity token: %w", err)
	}

	s.user = models.NewUser(claims, tokenStr)
	s.logger.Debug().Msg("Identity token loaded: " + s.user.String())

	return nil
}

// watchIdentityToken polls the identity token file at a fixed interval.  When
// it detects that the token is no longer valid (expired or unreadable) it sends
// a human-readable shutdown reason to the provided channel and returns.
//
// The goroutine also returns cleanly when ctx is cancelled so that normal
// signal-driven shutdowns do not leave it running.
func (s *Server) watchIdentity(ctx context.Context, shutdown chan<- string) {
	ticker := time.NewTicker(identityRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if reason := s.refreshIdentity(); reason != "" {
				s.logger.Warn().Msgf("Identity token refresh failed: %s", reason)
				select {
				case shutdown <- reason:
				default:
				}
				return
			}
		}
	}
}

// checkIdentityToken reads the current identity token file and verifies it
// using the stored JWTVerifier.  Returns a non-empty reason string when the
// token is expired or otherwise invalid, empty string when all is well.
func (s *Server) refreshIdentity() string {
	data, err := os.ReadFile(s.config.Identity.TokenPath)
	if err != nil {
		return fmt.Sprintf("failed to read identity token: %v", err)
	}

	tokenStr := strings.TrimSpace(string(data))
	token, err := s.jwtVerifier.VerifyToken(tokenStr)
	if err != nil {
		//return fmt.Sprintf("identity token is no longer valid: %v", err)
		s.logger.Error().Err(err).Msg("Identity token is no longer valid")
		return ""
	}

	updated, err := s.user.Update(token, tokenStr)
	if err != nil {
		return fmt.Sprintf("failed to update user information from new token: %v", err)
	}
	if updated {
		s.logger.Info().Msg("Identity token has been refreshed, will expire at: " + token.ExpiresAt.Time.UTC().Format(time.RFC3339))
	}

	return ""
}
