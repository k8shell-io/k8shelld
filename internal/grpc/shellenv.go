// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package grpc

import (
	"os"
	"path/filepath"

	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
)

const fallbackShell = "/bin/sh"

// loginShell returns the user's login shell from /etc/passwd, or "" if it
// cannot be resolved.
func loginShell(username string) string {
	shell, err := system.GetUserLoginShell(username)
	if err != nil {
		return ""
	}
	return shell
}

// usableShell reports whether shell is an executable file that can serve as an
// interpreter. nologin/false entries in passwd are rejected so that they fall
// through to the next candidate.
func usableShell(shell string) bool {
	if shell == "" {
		return false
	}
	switch filepath.Base(shell) {
	case "nologin", "false":
		return false
	}
	fi, err := os.Stat(shell)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// resolveShell returns the first usable shell from candidates in order,
// falling back to /bin/sh if none is usable.
func resolveShell(candidates ...string) string {
	for _, c := range candidates {
		if usableShell(c) {
			return c
		}
	}
	return fallbackShell
}

// userEnv builds the environment for a process started for shellUser. Client
// supplied variables win over the SHELL/USER/LOGNAME defaults and over
// k8shelld's own environment (PATH, LANG, ...); HOME is always the user's home.
func userEnv(reqEnv []string, shellUser models.ShellUser, shell string) []string {
	env := make([]string, 0, len(reqEnv)+3)
	env = append(env, reqEnv...)
	env = append(env,
		"SHELL="+shell,
		"USER="+shellUser.Username,
		"LOGNAME="+shellUser.Username,
	)
	return system.CreateEnvVars(env, shellUser.HomeDir)
}
