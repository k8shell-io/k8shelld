package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/models"
)

// applyIdentityToConfig reads the identity JWT from the path configured in
// cfg.Identity, verifies it using the configured public key and signing method,
// and populates cfg.User with the verified claims.
//
// Returns the JWTVerifier so the caller can hand it to the server for runtime
// token-renewal monitoring.  Returns nil, nil in test mode (secrets are not
// present in that environment).
func applyIdentityToConfig(cfg *config.Config, testMode bool) (*authz.JWTVerifier, error) {
	if testMode {
		return nil, nil
	}

	if cfg.Identity.TokenPath == "" || cfg.Identity.PublicKeyPath == "" || cfg.Identity.SigningMethod == "" {
		return nil, fmt.Errorf("identity config is incomplete: tokenPath, publicKeyPath and signingMethod are all required")
	}

	user, verifier, err := loadIdentityUser(cfg.Identity)
	if err != nil {
		return nil, err
	}

	// Populate identity fields from JWT claims.  Shell, Sudo, Groups and
	// HomeDir are not carried in the token; accessor methods on models.User
	// supply safe defaults for those.
	cfg.User.Username = user.Username
	cfg.User.Uid = user.Uid
	cfg.User.Gid = user.Gid
	cfg.User.Fullname = user.Fullname
	cfg.User.UserToken = user.UserToken

	return verifier, nil
}

// loadIdentityUser reads and verifies the identity JWT using the provided
// Identity config, returning a models.User populated from the token claims
// together with the JWTVerifier instance.
func loadIdentityUser(id config.Identity) (models.User, *authz.JWTVerifier, error) {
	tokenBytes, err := os.ReadFile(id.TokenPath)
	if err != nil {
		return models.User{}, nil, fmt.Errorf("read identity token: %w", err)
	}
	tokenStr := strings.TrimSpace(string(tokenBytes))

	verifier, err := authz.NewJWTVerifier(authz.JWTVerifierConfig{
		SigningMethod: id.SigningMethod,
		PublicKeyFile: id.PublicKeyPath,
	})
	if err != nil {
		return models.User{}, nil, fmt.Errorf("create JWT verifier: %w", err)
	}

	claims, err := verifier.VerifyToken(tokenStr)
	if err != nil {
		return models.User{}, nil, fmt.Errorf("verify identity token: %w", err)
	}

	user := models.User{
		Username:  claims.Subject,
		Uid:       claims.UID,
		Gid:       claims.GID,
		Fullname:  claims.Name,
		UserToken: tokenStr,
	}
	return user, verifier, nil
}
