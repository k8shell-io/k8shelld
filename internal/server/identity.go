package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/k8shelld/internal/models"
)

const identityRefreshInterval = 15 * time.Second
const JWT_VERIFIER_SIGNING_METHOD_ENV = "JWT_VERIFIER_SIGNING_METHOD"
const JWT_VERIFIER_PUBLIC_KEY_ENV = "JWT_VERIFIER_PUBLIC_KEY"
const IDENTITY_TOKEN_ENV = "IDENTITY_TOKEN"

// newJWTVerifier creates a JWTVerifier based on environment variables.
func newJWTVerifier() (*authz.JWTVerifier, error) {
	signingMethod := strings.TrimSpace(os.Getenv(JWT_VERIFIER_SIGNING_METHOD_ENV))
	if signingMethod == "" {
		return nil, fmt.Errorf("identity signing method is required (set %s or identity.signingMethod in config)", JWT_VERIFIER_SIGNING_METHOD_ENV)
	}
	publicKey := strings.TrimSpace(os.Getenv(JWT_VERIFIER_PUBLIC_KEY_ENV))
	if publicKey == "" {
		return nil, fmt.Errorf(" %s environment variable is required", JWT_VERIFIER_PUBLIC_KEY_ENV)
	}
	jwtCfg := authz.JWTVerifierConfig{SigningMethod: signingMethod}
	if signingMethod == "hs256" {
		jwtCfg.SecretKey = publicKey
	} else {
		jwtCfg.PublicKey = publicKey
	}
	jwtVerifier, err := authz.NewJWTVerifier(jwtCfg)
	if err != nil {
		return nil, fmt.Errorf("create JWT verifier: %w", err)
	}
	return jwtVerifier, nil
}

// loadIdentity reads the identity JWT from the IDENTITY_TOKEN environment
// variable, verifies it and initialises s.user with the verified claims.
func (s *Server) loadIdentity() error {
	if s.testMode {
		return nil
	}

	tokenStr := strings.TrimSpace(os.Getenv(IDENTITY_TOKEN_ENV))
	if tokenStr == "" {
		return fmt.Errorf(" %s environment variable is required", IDENTITY_TOKEN_ENV)
	}

	claims, err := s.jwtVerifier.VerifyToken(tokenStr)
	if err != nil {
		return fmt.Errorf("verify identity token: %w", err)
	}

	s.user = models.NewUser(claims, tokenStr)
	if s.apiClientx != nil {
		s.apiClientx.UpdateToken(tokenStr)
	}
	s.logger.Debug().Msg("Identity token loaded: " + s.user.String())

	return nil
}

// watchIdentity monitors the in-memory token expiry at a fixed interval.
// When the current token has expired it sends a shutdown reason and returns.
// The goroutine exits cleanly when ctx is cancelled.
func (s *Server) watchIdentity(ctx context.Context) {
	ticker := time.NewTicker(identityRefreshInterval)
	expiryReported := false
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if reason := s.checkTokenExpiry(); reason != "" {
				if !expiryReported {
					s.logger.Warn().Msgf("Identity token expired: %s", reason)
					expiryReported = true
				}
			} else if expiryReported {
				s.logger.Info().Msg("Identity token is valid again")
				expiryReported = false
			}
		}
	}
}

// checkTokenExpiry returns a non-empty reason string when the current in-memory
// token is expired, empty string when all is well.
func (s *Server) checkTokenExpiry() string {
	tokenStr := s.user.GetUserToken()
	if _, err := s.jwtVerifier.VerifyToken(tokenStr); err != nil {
		return fmt.Sprintf("identity token is no longer valid: %v", err)
	}
	return ""
}

// RefreshFromToken is called on every Handshake to replace the in-memory
// token with the one provided by the client. It verifies the token, then
// calls User.Update to atomically swap claims and propagate sudo changes.
func (s *Server) RefreshFromToken(tokenStr string) error {
	if s.jwtVerifier == nil {
		return nil // test mode
	}

	claims, err := s.jwtVerifier.VerifyToken(tokenStr)
	if err != nil {
		return fmt.Errorf("verify token: %w", err)
	}

	updated, err := s.user.Update(claims, tokenStr)
	if err != nil {
		return fmt.Errorf("update user from refresh token: %w", err)
	}
	if updated {
		s.logger.Info().Msgf("Identity token refreshed, expires at: %s",
			claims.ExpiresAt.Time.UTC().Format(time.RFC3339))
		if s.apiClientx != nil {
			s.apiClientx.UpdateToken(tokenStr)
		}
	}

	return nil
}
