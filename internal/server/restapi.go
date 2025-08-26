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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/k8shell-io/k8shelld/internal/grpc"
	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/rs/zerolog"
)

const APIServerBaseUrl = "http://api-internal/api/v1"

type RESTService struct {
	unixSocketPath string
	user           system.User
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
func NewRESTService(unixSocketPath string, user system.User, server *Server) (*RESTService, error) {
	logger := log.NewLogger("api")

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

	// Add token middleware
	apiRouter := router.PathPrefix("/api/v1").Subrouter()

	// Define API endpoints
	apiRouter.HandleFunc("/creds", a.GetCredsHelper).Methods(http.MethodGet)
	apiRouter.HandleFunc("/ssh/channels", a.GetSSHChannels).Methods(http.MethodGet)
	apiRouter.HandleFunc("/sysinfo", a.GetSystemInfo).Methods(http.MethodGet)
	apiRouter.HandleFunc("/logs", a.GetLogs).Methods(http.MethodGet)
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

// MakeApiServerRequest makes an HTTP request to the upstream API server
func (a *RESTService) MakeApiServerRequest(method string, url string, headers map[string]string) (string, error) {
	workspace := os.Getenv("WORKSPACE")
	fullURL := fmt.Sprintf("%s/workspaces/%s/%s", APIServerBaseUrl, workspace, url)

	req, err := http.NewRequest(method, fullURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create API server request: %v", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", a.user.UserToken))
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
func (a *RESTService) loggingMiddleware(next http.Handler) http.Handler {
	skipPaths := map[string]bool{
		// we need to skip logs as otherwise http.Flusher will not work
		"/api/v1/logs": true,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skipPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		a.logger.Debug().Msgf("Request: method %s, path %s, qs: %s", r.Method,
			r.URL.Path, r.URL.RawQuery)
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.logger.Debug().Msgf("Response: status %d, body: %s", rec.statusCode,
			sanitizeLogMessage(rec.body.String()))
	})
}

func (a *RESTService) GetCredsHelper(w http.ResponseWriter, r *http.Request) {
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

	url := fmt.Sprintf("%s/creds?address=%s", credsType, address)
	headers := map[string]string{"Accept": "application/json"}

	creds, err := a.MakeApiServerRequest("GET", url, headers)
	if err != nil {
		a.logger.Warn().Msgf("Cannot retrieve address for %s credential helper when calling upstream API %s: %v",
			credsType, url, err)
		http.Error(w, "Failed to retrieve credentials", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(creds))
}

func (a *RESTService) GetSSHChannels(w http.ResponseWriter, r *http.Request) {
	response, err := a.server.grpcService.GetAllChannelStoreData()
	if err != nil {
		a.logger.Error().Msgf("Failed to get channels data: %v", err)
		http.Error(w, "Failed to get channels data", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	w.WriteHeader(http.StatusOK)
}

func (a *RESTService) GetSystemInfo(w http.ResponseWriter, r *http.Request) {
	a.server.sysInfoMu.Lock()
	defer a.server.sysInfoMu.Unlock()

	uptime, err := system.GetStartTimeFromProcStat()
	if err != nil {
		a.logger.Error().Msgf("Failed to get uptime: %v", err)
		http.Error(w, "Failed to get uptime", http.StatusInternalServerError)
		return
	}

	var sysInfo system.SystemInfo
	if a.server.sysInfo != nil {
		sysInfo = *a.server.sysInfo
	}

	var users int = 0
	a.server.grpcService.SessionStore.Range(func(key, value any) bool {
		record, ok := value.(*grpc.SessionData)
		if ok && record.Deleted.UTC().IsZero() {
			users += 1
		}
		return true
	})

	response := models.SystemInfoResponse{
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
			n = 0

			if !follow {
				return
			}

			time.Sleep(100 * time.Millisecond)
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

// compile once for efficiency
var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)"?(password|secret|token)"?\s*:\s*"[^"]*"`),
	regexp.MustCompile(`(?i)(password|secret|token)\s*=\s*[^&\s]+`), // e.g. in query string
}

func sanitizeLogMessage(s string) string {
	for _, re := range sensitivePatterns {
		s = re.ReplaceAllStringFunc(s, func(match string) string {
			parts := strings.SplitN(match, ":", 2)
			if len(parts) < 2 {
				parts = strings.SplitN(match, "=", 2)
			}
			if len(parts) == 2 {
				return parts[0] + ":\"****\""
			}
			return match
		})
	}
	return s
}
