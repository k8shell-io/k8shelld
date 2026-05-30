package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/k8shelld/internal/models"
)

const identityRefreshInterval = 15 * time.Second
const identityRenewBeforeExpiry = 2 * time.Minute
const JWT_VERIFIER_SIGNING_METHOD_ENV = "JWT_VERIFIER_SIGNING_METHOD"
const JWT_VERIFIER_PUBLIC_KEY_ENV = "JWT_VERIFIER_PUBLIC_KEY"
const USER_UID_ENV = "USER_UID"
const USER_GID_ENV = "USER_GID"
const USER_DISPLAY_NAME_ENV = "USERFULLNAME"
const USER_EMAIL_ENV = "USEREMAIL"

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
		decoded, err := base64.StdEncoding.DecodeString(publicKey)
		if err != nil {
			return nil, fmt.Errorf("base64-decode %s: %w", JWT_VERIFIER_PUBLIC_KEY_ENV, err)
		}
		jwtCfg.PublicKey = string(decoded)
	}
	jwtVerifier, err := authz.NewJWTVerifier(jwtCfg)
	if err != nil {
		return nil, fmt.Errorf("create JWT verifier: %w", err)
	}
	return jwtVerifier, nil
}

// loadIdentity retrieves the identity JWT from the API server, verifies it and
// initializes s.user with the verified claims.
func (s *Server) loadIdentity() error {
	if s.testMode {
		return nil
	}
	if s.apiClientx == nil {
		s.logger.Warn().Msg("API server is not enabled, loading identity from environment variables")
		uidStr := strings.TrimSpace(os.Getenv(USER_UID_ENV))
		if uidStr == "" {
			return fmt.Errorf("API server is disabled but %s is not set", USER_UID_ENV)
		}
		gidStr := strings.TrimSpace(os.Getenv(USER_GID_ENV))
		if gidStr == "" {
			return fmt.Errorf("API server is disabled but %s is not set", USER_GID_ENV)
		}
		uid64, err := strconv.ParseUint(uidStr, 10, 32)
		if err != nil {
			return fmt.Errorf("parse %s=%q: %w", USER_UID_ENV, uidStr, err)
		}
		gid64, err := strconv.ParseUint(gidStr, 10, 32)
		if err != nil {
			return fmt.Errorf("parse %s=%q: %w", USER_GID_ENV, gidStr, err)
		}
		claims := &authz.UserClaims{UID: uint32(uid64), GID: uint32(gid64)}
		claims.Subject = s.username
		claims.Name = strings.TrimSpace(os.Getenv(USER_DISPLAY_NAME_ENV))
		if claims.Name == "" {
			claims.Name = s.username
		}
		claims.Email = strings.TrimSpace(os.Getenv(USER_EMAIL_ENV))
		s.user = models.NewUser(claims, "")
		s.logger.Debug().Msgf("Environment identity loaded: uid=%d gid=%d", uid64, gid64)
		return nil
	}

	tokenStr, err := s.apiClientx.IssueUserToken(context.Background(), s.username)
	if err != nil {
		return fmt.Errorf("issue identity token for user %s: %w", s.username, err)
	}

	claims, err := s.jwtVerifier.VerifyToken(tokenStr)
	if err != nil {
		return fmt.Errorf("verify identity token: %w", err)
	}

	if claims.Subject != s.username {
		return fmt.Errorf("issued token subject %q does not match workspace user %q", claims.Subject, s.username)
	}

	s.user = models.NewUser(claims, tokenStr)
	s.apiClientx.UpdateToken(tokenStr)
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
			if err := s.renewIdentityTokenIfNeeded(ctx); err != nil {
				s.logger.Warn().Msgf("Failed to renew identity token: %v", err)
			}
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

func (s *Server) renewIdentityTokenIfNeeded(ctx context.Context) error {
	if s.apiClientx == nil || s.user == nil {
		return nil
	}

	if time.Until(s.user.ClaimsSnapshot().ExpiresAt.Time) > identityRenewBeforeExpiry {
		return nil
	}

	username := s.user.GetUsername()
	tokenStr, err := s.apiClientx.IssueUserToken(ctx, username)
	if err != nil {
		return fmt.Errorf("issue token for user %s: %w", username, err)
	}

	claims, err := s.jwtVerifier.VerifyToken(tokenStr)
	if err != nil {
		return fmt.Errorf("verify token: %w", err)
	}

	_, err = s.user.Update(claims, tokenStr)
	if err != nil {
		return fmt.Errorf("update user from refresh token: %w", err)
	}

	s.logger.Info().Msgf("Identity token refreshed, expires at: %s",
		claims.ExpiresAt.Time.UTC().Format(time.RFC3339))
	s.apiClientx.UpdateToken(tokenStr)

	return nil
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
