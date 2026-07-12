// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/k8shelld/internal/models"
)

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

// loadIdentity initializes s.user from the USER_UID/USER_GID environment
// variables. Identity is no longer fetched from the API server: there is no
// per-user token to issue or renew, so the workspace identity comes straight
// from the environment regardless of whether the API server is enabled.
func (s *Server) loadIdentity() error {
	if s.testMode {
		return nil
	}
	uidStr := strings.TrimSpace(os.Getenv(USER_UID_ENV))
	if uidStr == "" {
		return fmt.Errorf("%s is not set", USER_UID_ENV)
	}
	gidStr := strings.TrimSpace(os.Getenv(USER_GID_ENV))
	if gidStr == "" {
		return fmt.Errorf("%s is not set", USER_GID_ENV)
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
