// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"fmt"

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/models"
)

// loadProfile initializes s.user from the workspace user's profile at
// /etc/k8shell/profile.yaml. Like blueprint.yaml, the file is placed there by
// the k8Shell provisioner — k8shelld never writes it.
func (s *Server) loadProfile() error {
	if s.testMode {
		return nil
	}

	profile, err := config.LoadProfile(config.ProfilePath)
	if err != nil {
		return fmt.Errorf("load profile from %s: %w", config.ProfilePath, err)
	}
	if profile.Username != s.username {
		return fmt.Errorf("profile username %q does not match workspace user %q", profile.Username, s.username)
	}

	s.user = models.NewUser(profile)
	s.logger.Debug().Msg("User profile loaded: " + s.user.String())
	return nil
}
