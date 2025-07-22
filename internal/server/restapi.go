package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/k8shell-io/k8shelld/internal/common"
	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/rs/zerolog"
)

const APIServerBaseUrl = "http://api-internal/api/v1"

type RESTApiService struct {
	apiServerToken string // Token for API server authentication
	unixSocketPath string
	user           User
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

// NewRESTAPI creates a new REST API service
func NewRESTAPI(apiServerToken string, unixSocketPath string, user User, server *Server) (*RESTApiService, error) {
	logger := log.NewLogger("api")

	return &RESTApiService{
		apiServerToken: apiServerToken,
		unixSocketPath: unixSocketPath,
		user:           user,
		logger:         logger,
		server:         server,
	}, nil
}

// Initialize the router
func (a *RESTApiService) initializeRouter() *mux.Router {
	router := mux.NewRouter()

	router.Use(a.loggingMiddleware)

	// Add token middleware
	apiRouter := router.PathPrefix("/api/v1").Subrouter()

	// Define API endpoints
	apiRouter.HandleFunc("/docker/dns", a.UpdateDockerDNS).Methods(http.MethodPatch)
	apiRouter.HandleFunc("/docker/dns", a.GetDockerDNS).Methods(http.MethodGet)
	apiRouter.HandleFunc("/docker/creds-helper", a.GetDockerCredsHelper).Methods(http.MethodGet)
	apiRouter.HandleFunc("/ssh/channels", a.GetSSHChannels).Methods(http.MethodGet)
	apiRouter.HandleFunc("/sysinfo", a.GetSystemInfo).Methods(http.MethodGet)
	apiRouter.HandleFunc("/logs", a.GetLogs).Methods(http.MethodGet)
	a.logRoutes(router)
	return router
}

// logRoutes logs all registered routes in the router
func (a *RESTApiService) logRoutes(router *mux.Router) {
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

// MakeApiServerRequest makes an HTTP request to the upstream API server
func (a *RESTApiService) MakeApiServerRequest(method string, url string, headers map[string]string) (string, error) {
	workspace := os.Getenv("WORKSPACE")
	fullURL := fmt.Sprintf("%s/workspaces/%s/%s", APIServerBaseUrl, workspace, url)

	req, err := http.NewRequest(method, fullURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create API server request: %v", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", a.apiServerToken))
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client := &http.Client{Timeout: 1000 * time.Millisecond}

	sanitizedHeaders := make(map[string]string)
	for key, values := range req.Header {
		if strings.ToLower(key) == "authorization" {
			sanitizedHeaders[key] = "***"
		} else {
			sanitizedHeaders[key] = values[0]
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errorResponse struct {
			Errno   int    `json:"errno"`
			Message string `json:"message"`
		}

		if err := json.Unmarshal(body, &errorResponse); err != nil {
			return string(body), fmt.Errorf("API call failed with status %d: %s", resp.StatusCode, body)
		}

		return string(body), fmt.Errorf("%s", errorResponse.Message)
	}

	return string(body), nil
}

// Middleware to log requests and responses
func (a *RESTApiService) loggingMiddleware(next http.Handler) http.Handler {
	skipPaths := map[string]bool{
		// we need to skip logs as otherwise http.Flusher will not work
		"/api/v1/logs": true,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skipPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		a.logger.Debug().Msgf("Request: method %s, path %s", r.Method, r.URL.Path)
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.logger.Debug().Msgf("Response: status %d, body: %s", rec.statusCode, rec.body.String())
	})
}

func (a *RESTApiService) UpdateDockerDNS(w http.ResponseWriter, r *http.Request) {
	var dnsRequest DockerDNSRequest

	err := json.NewDecoder(r.Body).Decode(&dnsRequest)
	if err != nil {
		a.logger.Error().Msgf("Invalid request body: %v", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if a.server.dns != nil {
		switch dnsRequest.Status {
		case "enabled":
			a.server.dns.Enable()
		case "disabled":
			a.server.dns.Disable()
		default:
			http.Error(w, "Invalid status", http.StatusBadRequest)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (a *RESTApiService) GetDockerDNS(w http.ResponseWriter, r *http.Request) {
	var status string
	if a.server.dns != nil {
		if a.server.dns.enabled {
			status = "enabled"
		} else {
			status = "disabled"
		}
	} else {
		status = "n/a"
	}
	response := DockerDNSResponse{
		Status: status,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	w.WriteHeader(http.StatusOK)
}

func (a *RESTApiService) GetDockerCredsHelper(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	if address == "" {
		http.Error(w, "Missing 'address' query parameter", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("registry/creds?address=%s", address)
	headers := map[string]string{"Accept": "application/json"}

	creds, err := a.MakeApiServerRequest("GET", url, headers)
	if err != nil {
		a.logger.Warn().Msgf("Cannot retrieve address for docker credential helper: %v", err)
		http.Error(w, "Failed to retrieve credentials", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(creds))
}

func (a *RESTApiService) GetSSHChannels(w http.ResponseWriter, r *http.Request) {
	response, err := a.server.grpcApi.getAllChannelStoreData()
	if err != nil {
		a.logger.Error().Msgf("Failed to get channels data: %v", err)
		http.Error(w, "Failed to get channels data", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	w.WriteHeader(http.StatusOK)
}

func (a *RESTApiService) GetSystemInfo(w http.ResponseWriter, r *http.Request) {
	a.server.sysInfoMu.Lock()
	defer a.server.sysInfoMu.Unlock()

	uptime, err := GetStartTimeFromProcStat()
	if err != nil {
		a.logger.Error().Msgf("Failed to get uptime: %v", err)
		http.Error(w, "Failed to get uptime", http.StatusInternalServerError)
		return
	}

	var sysInfo SystemInfo
	if a.server.sysInfo != nil {
		sysInfo = *a.server.sysInfo
	}

	var users int = 0
	a.server.grpcApi.sessionStore.Range(func(key, value any) bool {
		record, ok := value.(*SessionData)
		if ok && record.Deleted.UTC().IsZero() {
			users += 1
		}
		return true
	})

	response := common.SystemInfoResponse{
		Uptime:             uptime.Format(time.RFC3339),
		CPUUsageMillicores: sysInfo.CPUUsageMillicores,
		CPULimitMillicores: sysInfo.CPULimitMillicores,
		MemoryUsageMiB:     sysInfo.MemoryUsageMiB,
		MemLimitMiB:        sysInfo.MemLimitMiB,
		CPUAvg1Min:         math.Round(sysInfo.CPUAvg1Min*100) / 100,
		CPUAvg5Min:         math.Round(sysInfo.CPUAvg5Min*100) / 100,
		CPUAvg15Min:        math.Round(sysInfo.CPUAvg15Min*100) / 100,
		Users:              users,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	w.WriteHeader(http.StatusOK)
}

func (a *RESTApiService) GetLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	component := r.URL.Query().Get("component")
	level := r.URL.Query().Get("level")
	follow := r.URL.Query().Get("follow") == "true"

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	offset := 0
	for {
		entries, newOffset := log.LogStore.GetLogsSince(offset, component, level)

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
			break
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func (a *RESTApiService) Handler(ctx context.Context) {
	router := a.initializeRouter()
	if a.unixSocketPath != "" {
		go a.manageUnixSocket(ctx, router)
	}
}

func (a *RESTApiService) manageUnixSocket(ctx context.Context, router http.Handler) {
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

		err = os.Chown(a.unixSocketPath, a.user.Uid, a.user.Gid)
		if err != nil {
			a.logger.Error().Msgf("Error changing ownership of Unix socket: %v", err)
			unixListener.Close()
			os.Remove(a.unixSocketPath)
			time.Sleep(5 * time.Second)
			continue
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
			server.Shutdown(context.Background())
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
