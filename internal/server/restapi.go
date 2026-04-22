package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	commonModels "github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/k8shelld/internal/apps"
	"github.com/k8shell-io/k8shelld/internal/config"
	grpcpkg "github.com/k8shell-io/k8shelld/internal/grpc"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

const API_VERSION = "v1"

type RESTService struct {
	unixSocketPath string
	user           *models.User
	logger         *zerolog.Logger
	server         *Server
}

// responseRecorder is a wrapper for http.ResponseWriter
// to capture the status code and response body.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
}

// WriteHeader captures the status code and forwards it to the original ResponseWriter
func (rec *responseRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

// Write captures the response body and writes it to the original ResponseWriter
func (rec *responseRecorder) Write(data []byte) (int, error) {
	rec.body.Write(data)
	return rec.ResponseWriter.Write(data)
}

// Hijack implements http.Hijacker by delegating to the underlying ResponseWriter,
// allowing handlers like AttachShell to take over the raw connection.
func (rec *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := rec.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

// NewRESTAPI creates a new REST API service
func NewRESTService(unixSocketPath string, user *models.User, server *Server) (*RESTService, error) {
	logger := logger.NewLogger("api")

	return &RESTService{
		unixSocketPath: unixSocketPath,
		user:           user,
		logger:         logger,
		server:         server,
	}, nil
}

// Initialize the router
func (a *RESTService) initializeRouter() *mux.Router {
	router := mux.NewRouter()

	router.Use(a.loggingMiddleware)

	apiRouter := router.PathPrefix("/api/v1").Subrouter()
	apiRouter.HandleFunc("/creds", a.GetCredsHelper).Methods(http.MethodGet)
	apiRouter.HandleFunc("/sessions", a.GetSessions).Methods(http.MethodGet)
	apiRouter.HandleFunc("/ssh/channels", a.GetSSHChannels).Methods(http.MethodGet)
	apiRouter.HandleFunc("/sysinfo", a.GetSystemInfo).Methods(http.MethodGet)
	apiRouter.HandleFunc("/logs", a.GetLogs).Methods(http.MethodGet)
	apiRouter.HandleFunc("/shutdown", a.Shutdown).Methods(http.MethodPost)
	apiRouter.HandleFunc("/validate", a.ValidateK8shelldFile).Methods(http.MethodPost)
	apiRouter.HandleFunc("/apps", a.GetAppsStatus).Methods(http.MethodGet)
	apiRouter.HandleFunc("/apps/{name}/install", a.InstallApp).Methods(http.MethodPost)
	apiRouter.HandleFunc("/apps/{name}/logs", a.GetAppLogs).Methods(http.MethodGet)
	apiRouter.HandleFunc("/apps/{name}/start", a.StartApp).Methods(http.MethodPost)
	apiRouter.HandleFunc("/apps/{name}/stop", a.StopApp).Methods(http.MethodPost)
	apiRouter.HandleFunc("/identity", a.GetIdentity).Methods(http.MethodGet)
	apiRouter.HandleFunc("/splash", a.GetSplash).Methods(http.MethodGet)
	apiRouter.HandleFunc("/shells", a.ListDetachedShells).Methods(http.MethodGet)
	apiRouter.HandleFunc("/shells/{id}/detach", a.DetachShell).Methods(http.MethodPost)
	apiRouter.HandleFunc("/shells/{id}/attach", a.AttachShell).Methods(http.MethodPost)
	apiRouter.HandleFunc("/shells/{id}/resize", a.ResizeShell).Methods(http.MethodPost)

	a.logRoutes(router)
	return router
}

// logRoutes logs all registered routes in the router
func (a *RESTService) logRoutes(router *mux.Router) {
	err := router.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		path, err := route.GetPathTemplate()
		if err != nil {
			path = "<undefined>"
		}

		methods, err := route.GetMethods()
		if err != nil {
			methods = []string{"<any>"}
		}

		a.logger.Debug().Msgf("Route: %s Methods: %v", path, methods)
		return nil
	})

	if err != nil {
		a.logger.Info().Msgf("Error walking routes: %v", err)
	}
}

// Middleware to log requests and responses
func (a *RESTService) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/logs" ||
			(strings.HasPrefix(r.URL.Path, "/api/v1/apps/") &&
				strings.HasSuffix(r.URL.Path, "/logs")) {
			next.ServeHTTP(w, r)
			return
		}

		a.logger.Debug().Msgf("Request: method %s, path %s, qs: %s", r.Method, r.URL.Path, r.URL.RawQuery)
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.logger.Debug().Msgf("Response: status %d", rec.statusCode)
	})
}

func (a *RESTService) GetSessions(w http.ResponseWriter, r *http.Request) {
	if a.server.apiClientx == nil {
		http.Error(w, "API server not configured.", http.StatusBadRequest)
		return
	}

	num := r.URL.Query().Get("num")
	if num == "" {
		num = "10"
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 || n > 100 {
		http.Error(w, "Invalid 'num' parameter, must be between 1 and 100", http.StatusBadRequest)
		return
	}

	a.logger.Debug().Msgf("Fetching last %d sessions for workspace %s", n, a.server.workspace)
	sessions, err := a.server.apiClientx.ListUserSessions(r.Context(), a.user.GetUsername(),
		a.server.workspace, n, 0, true)
	if err != nil {
		a.logger.Warn().Msgf("Cannot retrieve workspace sessions: %v", err)
		http.Error(w, "Failed to retrieve sessions", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sessions); err != nil {
		a.logger.Error().Msgf("Failed to encode sessions response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

func (a *RESTService) Shutdown(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	if action == "" {
		action = "stop"
	}
	if action != "stop" && action != "delete" {
		http.Error(w, "invalid action: must be 'stop' or 'delete'", http.StatusBadRequest)
		return
	}
	a.logger.Debug().Msgf("Shutting down workspace %s (action=%s)", a.server.workspace, action)
	_, err := a.server.grpcService.CommandService.SendCommand(r.Context(), "shutdown "+action)
	if err != nil {
		a.logger.Warn().Msgf("Cannot shutdown workspace: %v", err)
		http.Error(w, "Failed to shutdown workspace", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *RESTService) GetCredsHelper(w http.ResponseWriter, r *http.Request) {
	if a.server.apiClientx == nil {
		http.Error(w, "API server not configured.", http.StatusBadRequest)
		return
	}

	address := r.URL.Query().Get("address")
	if address == "" {
		http.Error(w, "Missing 'address' query parameter", http.StatusBadRequest)
		return
	}
	credsType := r.URL.Query().Get("type")
	if credsType == "" {
		http.Error(w, "Missing 'type' query parameter", http.StatusBadRequest)
		return
	}
	if credsType != "docker" && credsType != "git" {
		http.Error(w, "Invalid 'type' query parameter, must be 'docker' or 'git'", http.StatusBadRequest)
		return
	}

	a.logger.Debug().Msgf("Fetching %s credentials for address %s and user %s", credsType,
		address, a.user.GetUsername())

	creds, err := a.server.apiClientx.GetUserCredentials(r.Context(), a.user.GetUsername())
	if err != nil {
		a.logger.Warn().Msgf("Cannot retrieve user credentials: %v", err)
		http.Error(w, "Failed to retrieve credentials", http.StatusBadGateway)
		return
	}

	for _, cred := range creds {
		a.logger.Debug().Msgf("Checking credential: ServiceName=%s, ServiceURL=%s, Username=%s",
			cred.ServiceName, cred.ServiceURL, cred.ExternalID)
		if credsType == "docker" && cred.ServiceName == "registry" && cred.ServiceURL == address {
			credStr := fmt.Sprintf(`{"ServerURL": "%s", "Username": "%s", "Secret": "%s"}`,
				cred.ServiceURL, cred.ExternalID, cred.ExternalToken)
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(credStr)); err != nil {
				a.logger.Error().Msgf("Failed to write credentials response: %v", err)
			}
			return
		}

		credAddress := cred.ServiceURL
		parts := strings.Split(cred.ServiceURL, "://")
		if len(parts) == 2 && !strings.HasPrefix(address, "http") {
			credAddress = parts[1]
		}

		if credsType == "git" && cred.ServiceName == "git" && credAddress == address {
			credStr := fmt.Sprintf(`{"Username": "%s", "Password": "%s"}`,
				cred.ExternalID, cred.ExternalToken)
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(credStr)); err != nil {
				a.logger.Error().Msgf("Failed to write credentials response: %v", err)
			}
			return
		}
	}

	a.logger.Warn().Msgf("No credentials found for address: %s and type: %s", address, credsType)
	http.Error(w, "Credentials not found", http.StatusNotFound)
}

func (a *RESTService) GetSSHChannels(w http.ResponseWriter, r *http.Request) {
	response, err := a.server.grpcService.GetAllChannelStoreData()
	if err != nil {
		a.logger.Error().Msgf("Failed to get channels data: %v", err)
		http.Error(w, "Failed to get channels data", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.logger.Error().Msgf("Failed to encode SSH channels response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

// repoURL builds a repository URL from GIT_ADDRESS, GIT_REPOOWNER and GIT_REPONAME env vars.
// Returns an empty string when any of the required vars is unset.
func repoURL() string {
	addr := strings.TrimRight(os.Getenv("GIT_ADDRESS"), "/")
	owner := os.Getenv("GIT_REPOOWNER")
	name := os.Getenv("GIT_REPONAME")
	if addr == "" || owner == "" || name == "" {
		return ""
	}
	return addr + "/" + owner + "/" + name
}

func (a *RESTService) GetSystemInfo(w http.ResponseWriter, r *http.Request) {
	metrics, err := a.server.sysInfo.GetSystemUsageSnapshot()
	if err != nil {
		a.logger.Error().Msgf("Failed to get system info metrics snapshot: %v", err)
		http.Error(w, "Failed to get system info metrics snapshot", http.StatusInternalServerError)
		return
	}
	metrics.Users = a.server.grpcService.NumSessions()

	mounts, err := a.server.sysInfo.GetMountUsageSnapshot()
	if err != nil {
		a.logger.Error().Msgf("Failed to get mount usage snapshot: %v", err)
		http.Error(w, "Failed to get mount usage snapshot", http.StatusInternalServerError)
		return
	}

	docker, err := a.server.sysInfo.GetDockerUsageSnapshot(r.Context())
	if err != nil {
		a.logger.Error().Msgf("Failed to get docker usage snapshot: %v", err)
		http.Error(w, "Failed to get docker usage snapshot", http.StatusInternalServerError)
		return
	}

	response := k8shelld.SystemInfo{
		Time:       time.Now().Format(time.RFC3339),
		System:     metrics,
		Mounts:     mounts,
		Docker:     docker,
		Repository: repoURL(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.logger.Error().Msgf("Failed to encode system info response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

func (a *RESTService) GetIdentity(w http.ResponseWriter, r *http.Request) {
	claims := a.user.ClaimsSnapshot()

	roles := make([]string, len(claims.Roles))
	for i, role := range claims.Roles {
		roles[i] = string(role)
	}

	expiresAt := ""
	if claims.ExpiresAt != nil {
		expiresAt = claims.ExpiresAt.Time.UTC().Format(time.RFC3339)
	}

	shell := claims.Shell
	if shell == "" {
		shell = "/bin/sh"
	}

	response := k8shelld.IdentityInfo{
		Username:     a.user.GetUsername(),
		Name:         claims.Name,
		Email:        claims.Email,
		UID:          a.user.GetUID(),
		GID:          a.user.GetGID(),
		Shell:        shell,
		Sudo:         claims.Sudo,
		Roles:        roles,
		Organization: claims.Organization,
		Source:       claims.Source,
		ExpiresAt:    expiresAt,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.logger.Error().Msgf("Failed to encode identity response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

func (a *RESTService) GetSplash(w http.ResponseWriter, r *http.Request) {
	var splash string
	if a.server.blueprint != nil {
		splash = a.server.blueprint.Splash
	}
	expanded := config.ExpandSplash(splash, a.user.GetUsername())
	// ExpandSplash returns PTY-style \r\n; normalise to plain \n for terminal.
	text := strings.ReplaceAll(expanded, "\r\n", "\n")

	response := k8shelld.SplashInfo{Text: text}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.logger.Error().Msgf("Failed to encode splash response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

func (a *RESTService) GetLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	component := r.URL.Query().Get("component")
	level := r.URL.Query().Get("level")
	follow := r.URL.Query().Get("follow") == "true"
	lastN := r.URL.Query().Get("lastN")

	var err error
	var n int
	if lastN != "" {
		n, err = strconv.Atoi(lastN)
		if err != nil || n < 0 {
			http.Error(w, "Invalid 'lastN' parameter", http.StatusBadRequest)
			return
		}
	} else {
		n = 0
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	offset := 0
	if n > 0 {
		offset = -n
	}

	for {
		select {
		case <-r.Context().Done():
			return
		default:
			entries, newOffset := logger.GetLogsSince(offset, component, level)

			for _, entry := range entries {
				b, err := json.Marshal(entry)
				if err != nil {
					a.logger.Error().Err(err).Msg("failed to encode log entry")
					continue
				}
				_, _ = fmt.Fprintln(w, string(b))
			}

			flusher.Flush()
			offset = newOffset

			if !follow {
				return
			}

			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (a *RESTService) ValidateK8shelldFile(w http.ResponseWriter, r *http.Request) {
	if a.server.apiClientx == nil {
		http.Error(w, "API server not configured.", http.StatusBadRequest)
		return
	}

	filename := r.URL.Query().Get("file")
	if filename == "" {
		http.Error(w, "Missing 'file' query parameter", http.StatusBadRequest)
		return
	}
	compose := r.URL.Query().Get("compose") == "true"

	a.logger.Debug().Msgf("Validating k8shell file: %s", filename)

	k8shellFileYAML, err := os.ReadFile(filename)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read file %s", filename), http.StatusBadRequest)
		return
	}

	var k8shellFile commonModels.K8shellFile
	if err := yaml.Unmarshal(k8shellFileYAML, &k8shellFile); err != nil {
		http.Error(w, fmt.Sprintf("Invalid YAML format: %v", err), http.StatusBadRequest)
		return
	}

	_, errors := commonModels.ValidateK8shellFile(k8shellFile)
	var response models.K8shellFileValidationResponse
	if len(errors) == 0 {
		response = models.K8shellFileValidationResponse{
			Status:   "valid",
			Filename: filename,
			Errors:   nil,
		}

		if compose {
			_, err := a.server.apiClientx.ComposeBlueprint(r.Context(), a.user.GetUsername(), &k8shellFile)
			if err != nil {
				response.Status = "invalid"
				response.Errors = []string{fmt.Sprintf("Failed to compose final blueprint: %v", err)}
			}
		}

	} else {
		w.Header().Set("Content-Type", "application/json")
		response = models.K8shellFileValidationResponse{
			Status:   "invalid",
			Filename: filename,
			Errors:   errors,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.logger.Error().Msgf("Failed to encode validation response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetAppsStatus returns the status of all configured apps.
func (a *RESTService) GetAppsStatus(w http.ResponseWriter, r *http.Request) {
	a.logger.Debug().Msg("Fetching apps status")

	if a.server == nil || a.server.appManager == nil {
		http.Error(w, "App manager not available", http.StatusBadRequest)
		return
	}

	statuses, err := a.server.appManager.ListAppStatus(r.Context())
	if err != nil {
		if errors.Is(err, apps.ErrNoAppsConfigured) {
			http.Error(w, "No apps configured", http.StatusNotFound)
		} else {
			a.logger.Error().Msgf("Failed to list app status: %v", err)
			http.Error(w, "Failed to list app status", http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(statuses); err != nil {
		a.logger.Error().Msgf("Failed to encode app status response: %v", err)
	}
}

// InstallApp installs the specified app (asynchronously).
func (a *RESTService) InstallApp(w http.ResponseWriter, r *http.Request) {
	if a.server == nil || a.server.appManager == nil {
		http.Error(w, "App manager not available", http.StatusBadRequest)
		return
	}

	vars := mux.Vars(r)
	name := vars["name"]
	if name == "" {
		http.Error(w, "Missing app name", http.StatusBadRequest)
		return
	}

	force := r.URL.Query().Get("force") == "true"
	a.logger.Info().Msgf("Installing app %s (force=%v)", name, force)

	if err := a.server.appManager.InstallAsync(r.Context(), name, force); err != nil {
		a.logger.Error().Msgf("Failed to start install for app %s: %v", name, err)
		http.Error(w, fmt.Sprintf("Failed to start install: %v", err), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// GetAppLogs returns (and can stream) the latest logs for a given app.
func (a *RESTService) GetAppLogs(w http.ResponseWriter, r *http.Request) {
	if a.server == nil || a.server.appManager == nil {
		http.Error(w, "App manager not available", http.StatusBadRequest)
		return
	}

	logType := r.URL.Query().Get("logType")
	if logType == "" {
		logType = "app"
	}

	if logType != "app" && logType != "install" {
		http.Error(w, "Invalid 'logType' parameter, must be 'app' or 'install'", http.StatusBadRequest)
		return
	}

	vars := mux.Vars(r)
	name := vars["name"]
	if name == "" {
		http.Error(w, "Missing app name", http.StatusBadRequest)
		return
	}

	follow := r.URL.Query().Get("follow") == "true"
	logPath, err := a.server.appManager.GetLastLogPath(name, logType)
	if err != nil {
		a.logger.Error().Msgf("Failed to get %s log path for app %s: %v", logType, name, err)
		http.Error(w, fmt.Sprintf("Failed to get %s log: %v", logType, err), http.StatusInternalServerError)
		return
	}
	if logPath == "" {
		http.Error(w, "No "+logType+" logs found", http.StatusNotFound)
		return
	}

	if !follow {
		logText, err := os.ReadFile(logPath)
		if err != nil {
			a.logger.Error().Msgf("Failed to read %s logs for app %s: %v", logType, name, err)
			http.Error(w, fmt.Sprintf("Failed to read %s logs: %v", logType, err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(logText)
		return
	}

	if (logType == "install" && !a.server.appManager.IsInstalling(name)) || (logType == "app" && !a.server.appManager.IsRunning(name)) {
		logText, err := os.ReadFile(logPath)
		if err != nil {
			a.logger.Error().Msgf("Failed to read %s logs for app %s: %v", logType, name, err)
			http.Error(w, fmt.Sprintf("Failed to read %s logs: %v", logType, err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(logText)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	f, err := os.Open(logPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to open log file: %v", err), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	reader := bufio.NewReader(f)

	for {
		select {
		case <-r.Context().Done():
			return
		default:
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				_, _ = w.Write([]byte(line))
				flusher.Flush()
			}
			if err != nil {
				if err == io.EOF {
					if (logType == "install" && !a.server.appManager.IsInstalling(name)) ||
						(logType == "app" && !a.server.appManager.IsRunning(name)) {
						return
					}
					time.Sleep(200 * time.Millisecond)
					continue
				}
				return
			}
		}
	}
}

func (a *RESTService) Serve(ctx context.Context) {
	router := a.initializeRouter()
	if a.unixSocketPath != "" {
		go a.manageUnixSocket(ctx, router)
	}
}

func (a *RESTService) manageUnixSocket(ctx context.Context, router http.Handler) {
	for {
		select {
		case <-ctx.Done():
			a.logger.Info().Msgf("Context cancelled, stopping Unix socket server loop.")
			os.Remove(a.unixSocketPath)
			return
		default:
		}

		a.logger.Warn().Msgf("Creating unix socket %s", a.unixSocketPath)
		unixListener, err := net.Listen("unix", a.unixSocketPath)
		if err != nil {
			a.logger.Error().Msgf("Error creating Unix socket listener: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if !a.server.testMode {
			err = os.Chown(a.unixSocketPath, int(a.user.GetUID()), int(a.user.GetGID()))
			if err != nil {
				a.logger.Error().Msgf("Error changing ownership of Unix socket: %v", err)
				unixListener.Close()
				os.Remove(a.unixSocketPath)
				time.Sleep(5 * time.Second)
				continue
			}
		}

		a.logger.Info().Msgf("Unix socket server started at %s", a.unixSocketPath)

		server := &http.Server{
			Handler: router,
		}

		errCh := make(chan error, 1)

		go func() {
			errCh <- server.Serve(unixListener)
		}()

		select {
		case <-ctx.Done():
			a.logger.Info().Msgf("Shutting down Unix socket server...")
			if err := server.Shutdown(context.Background()); err != nil {
				a.logger.Error().Msgf("Error shutting down server: %v", err)
			}
			unixListener.Close()
			return
		case err := <-errCh:
			if err != nil && err != http.ErrServerClosed {
				a.logger.Error().Msgf("Socket server error: %v", err)
			}
			a.logger.Warn().Msg("Unix socket server terminated. Restarting...")
			unixListener.Close()
			time.Sleep(5 * time.Second)
			continue
		}
	}
}

// StartApp starts supervising and running the specified app.
func (a *RESTService) StartApp(w http.ResponseWriter, r *http.Request) {
	if a.server == nil || a.server.appManager == nil {
		http.Error(w, "App manager not available", http.StatusBadRequest)
		return
	}

	vars := mux.Vars(r)
	name := vars["name"]
	if name == "" {
		http.Error(w, "Missing app name", http.StatusBadRequest)
		return
	}

	a.logger.Info().Msgf("Starting app %s", name)

	if err := a.server.appManager.Start(r.Context(), name); err != nil {
		a.logger.Error().Msgf("Failed to start app %s: %v", name, err)
		http.Error(w, fmt.Sprintf("Failed to start app: %v", err), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// StopApp stops supervising and (if running) stops the specified app.
func (a *RESTService) StopApp(w http.ResponseWriter, r *http.Request) {
	if a.server == nil || a.server.appManager == nil {
		http.Error(w, "App manager not available", http.StatusBadRequest)
		return
	}

	vars := mux.Vars(r)
	name := vars["name"]
	if name == "" {
		http.Error(w, "Missing app name", http.StatusBadRequest)
		return
	}

	a.logger.Info().Msgf("Stopping app %s", name)

	if err := a.server.appManager.Stop(r.Context(), name); err != nil {
		a.logger.Error().Msgf("Failed to stop app %s: %v", name, err)
		http.Error(w, fmt.Sprintf("Failed to stop app: %v", err), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ResizeShell resizes the PTY of a shell session (works whether attached or detached).
func (a *RESTService) ResizeShell(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var req struct {
		Width  uint32 `json:"width"`
		Height uint32 `json:"height"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := a.server.grpcService.ResizeSession(id, req.Width, req.Height); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListDetachedShells returns a JSON list of PTY shell sessions with no attached client.
func (a *RESTService) ListDetachedShells(w http.ResponseWriter, r *http.Request) {
	result := a.server.grpcService.ListDetachedSessions()
	if result == nil {
		result = []grpcpkg.DetachedSessionInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		a.logger.Error().Msgf("ListDetachedShells encode: %v", err)
	}
}

// DetachShell signals the currently attached client of a session to detach,
// keeping the shell process alive for later `kbox attach`.
func (a *RESTService) DetachShell(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if err := a.server.grpcService.DetachShellSession(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AttachShell hijacks the HTTP connection and bridges it directly to the PTY
// of the requested shell session (replaying scrollback first).
func (a *RESTService) AttachShell(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if code, err := a.server.grpcService.ValidateSessionForAttach(id); err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	// Drain any buffered bytes the HTTP server read ahead.
	if buf.Reader.Buffered() > 0 {
		extra := make([]byte, buf.Reader.Buffered())
		_, _ = buf.Read(extra)
		_ = extra // discard; client hasn't sent PTY input yet
	}
	// Switch to raw PTY protocol: send 200 OK then hand the conn to the loop.
	if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		a.logger.Error().Msgf("AttachShell write 200: %v", err)
		return
	}
	if err := a.server.grpcService.ServeRESTAttach(id, conn); err != nil {
		a.logger.Error().Msgf("AttachShell ServeRESTAttach: %v", err)
	}
}
