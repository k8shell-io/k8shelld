package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

const identityTokenCheckInterval = 30 * time.Second

// watchIdentityToken polls the identity token file at a fixed interval.  When
// it detects that the token is no longer valid (expired or unreadable) it sends
// a human-readable shutdown reason to the provided channel and returns.
//
// The goroutine also returns cleanly when ctx is cancelled so that normal
// signal-driven shutdowns do not leave it running.
func (s *Server) watchIdentityToken(ctx context.Context, shutdown chan<- string) {
	ticker := time.NewTicker(identityTokenCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if reason := s.checkIdentityToken(); reason != "" {
				s.logger.Warn().Msgf("Identity token check failed: %s", reason)
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
func (s *Server) checkIdentityToken() string {
	data, err := os.ReadFile(s.config.Identity.TokenPath)
	if err != nil {
		return fmt.Sprintf("failed to read identity token: %v", err)
	}

	tokenStr := strings.TrimSpace(string(data))
	if _, err := s.jwtVerifier.VerifyToken(tokenStr); err != nil {
		return fmt.Sprintf("identity token is no longer valid: %v", err)
	}

	return ""
}
