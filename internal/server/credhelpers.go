package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/k8shell-io/k8shelld/internal/system"
)

const (
	kubectlCredHelperBin = "kubectl-credential-k8shell"
)

// currentPodNamespace returns the Kubernetes namespace this pod is running in.
// It reads the projected service-account namespace file first, then falls back
// to the POD_NAMESPACE environment variable, and finally to "default".
func currentPodNamespace() string {
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	return "default"
}

// setupCredentialHelpers configures git, docker, and kubernetes credential helpers
// in the user's home directory based on which services are available.
func (s *Server) setupCredentialHelpers() {
	homeDir := s.user.GetHomeDir()

	if s.blueprint != nil && s.blueprint.Podman.Enabled {
		if err := s.setupDockerCredHelper(homeDir); err != nil {
			s.logger.Error().Msgf("Failed to set up docker credential helper: %v", err)
		} else {
			s.logger.Info().Msg("Docker credential helper configured")
		}
	}

	if _, err := exec.LookPath("git"); err == nil {
		if err := s.setupGitCredHelper(homeDir); err != nil {
			s.logger.Error().Msgf("Failed to set up git credential helper: %v", err)
		} else {
			s.logger.Info().Msg("Git credential helper configured")
		}
	}

	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if err := s.setupKubernetesCredHelper(homeDir); err != nil {
			s.logger.Error().Msgf("Failed to set up kubernetes credential helper: %v", err)
		} else {
			s.logger.Info().Msg("Kubernetes credential helper configured")
		}
	}
}

// setupDockerCredHelper creates ~/.docker/config.json if it does not already
// exist, pointing docker at the k8shell credential store.
func (s *Server) setupDockerCredHelper(homeDir string) error {
	dockerDir := filepath.Join(homeDir, ".docker")
	configPath := filepath.Join(dockerDir, "config.json")

	if _, err := os.Stat(configPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(dockerDir, 0o700); err != nil {
		return fmt.Errorf("create .docker directory: %w", err)
	}
	if err := os.Chown(dockerDir, int(s.user.GetUID()), int(s.user.GetGID())); err != nil {
		return fmt.Errorf("chown .docker directory: %w", err)
	}

	cfg := map[string]string{"credsStore": "k8shell"}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal docker config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		return fmt.Errorf("write docker config: %w", err)
	}
	return os.Chown(configPath, int(s.user.GetUID()), int(s.user.GetGID()))
}

// setupGitCredHelper writes git global configuration for the user:
// user.name, user.email, and credential.helper.
// It is always applied so that identity changes between sessions are reflected.
func (s *Server) setupGitCredHelper(homeDir string) error {
	claims := s.user.ClaimsSnapshot()

	name := claims.Name
	if name == "" {
		name = "n/a"
	}
	email := claims.Email
	if email == "" {
		email = "n/a"
	}

	gitConfigs := [][2]string{
		{"user.name", name},
		{"user.email", email},
		{"credential.helper", "k8shell"},
	}

	for _, kv := range gitConfigs {
		if err := s.runCommandAsUser(homeDir, "git", "config", "--global", kv[0], kv[1]); err != nil {
			return fmt.Errorf("git config %s: %w", kv[0], err)
		}
	}
	return nil
}

// setupKubernetesCredHelper creates ~/.kube/config with an exec credential
// plugin if one does not already exist. The API server address is derived from
// KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT environment variables.
func (s *Server) setupKubernetesCredHelper(homeDir string) error {
	kubeDir := filepath.Join(homeDir, ".kube")
	configPath := filepath.Join(kubeDir, "config")

	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if port == "" {
		port = "443"
	}
	server := fmt.Sprintf("https://%s:%s", host, port)

	clusterConfig := fmt.Sprintf("    server: %s", server)
	if caBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"); err == nil {
		caData := base64.StdEncoding.EncodeToString(caBytes)
		clusterConfig = fmt.Sprintf("    certificate-authority-data: %s\n    server: %s", caData, server)
	}

	namespace := currentPodNamespace()

	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
%s
  name: default
contexts:
- context:
    cluster: default
    namespace: %s
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: %s
      interactiveMode: IfAvailable
`, clusterConfig, namespace, kubectlCredHelperBin)

	if err := os.MkdirAll(kubeDir, 0o700); err != nil {
		return fmt.Errorf("create .kube directory: %w", err)
	}
	if err := os.Chown(kubeDir, int(s.user.GetUID()), int(s.user.GetGID())); err != nil {
		return fmt.Errorf("chown .kube directory: %w", err)
	}

	if err := os.WriteFile(configPath, []byte(kubeconfig), 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	return os.Chown(configPath, int(s.user.GetUID()), int(s.user.GetGID()))
}

// runCommandAsUser runs an executable as the workspace user (UID/GID) with
// HOME set to homeDir.
func (s *Server) runCommandAsUser(homeDir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = system.CreateEnvVars(nil, homeDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: s.user.GetUID(),
			Gid: s.user.GetGID(),
		},
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
