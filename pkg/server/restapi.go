package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

const APIServerBaseUrl = "http://api-internal/api/v1"

type RESTApiService struct {
	apiServerToken string // Token for API server authentication
	unixSocketPath string
	user           MainUser
	logger         *Logger
	grpcApi        *GRPCApiService
	dns            *DockerDNS
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

func NewRESTAPI(apiServerToken string, unixSocketPath string, user MainUser,
	grpcApi *GRPCApiService, dns *DockerDNS) (*RESTApiService, error) {

	logger := NewLogger("api")

	return &RESTApiService{
		apiServerToken: apiServerToken,
		unixSocketPath: unixSocketPath,
		user:           user,
		logger:         logger,
		grpcApi:        grpcApi,
		dns:            dns,
	}, nil
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.logger.Debug("Request: method %s, path %s", r.Method, r.URL.Path)
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.logger.Debug("Response: status %d, body: %s", rec.statusCode, rec.body.String())
	})
}

func (a *RESTApiService) LogMessage(w http.ResponseWriter, r *http.Request) {
	var message LogMessageRequest

	err := json.NewDecoder(r.Body).Decode(&message)
	if err != nil {
		a.logger.Error("Invalid request body: %v", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	_log := NewLogger(message.Component)
	if message.Level == "info" {
		_log.Info("%s", message.Message)
	} else if message.Level == "error" {
		_log.Error("%s", message.Message)
	} else if message.Level == "debug" {
		_log.Debug("%s", message.Message)
	} else if message.Level == "warn" {
		_log.Warn("%s", message.Message)
	} else {
		http.Error(w, "Invalid log level", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *RESTApiService) UpdateDockerDNS(w http.ResponseWriter, r *http.Request) {
	var dnsRequest DockerDNSRequest

	err := json.NewDecoder(r.Body).Decode(&dnsRequest)
	if err != nil {
		a.logger.Error("Invalid request body: %v", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if a.dns != nil {
		switch dnsRequest.Status {
		case "enabled":
			a.dns.Enable()
		case "disabled":
			a.dns.Disable()
		default:
			http.Error(w, "Invalid status", http.StatusBadRequest)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (a *RESTApiService) GetDockerDNS(w http.ResponseWriter, r *http.Request) {
	var status string
	if a.dns != nil {
		if a.dns.enabled {
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
		a.logger.Warn("Cannot retrieve address for docker credential helper: %v", err)
		http.Error(w, "Failed to retrieve credentials", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(creds))
}

func (a *RESTApiService) GetSSHChannels(w http.ResponseWriter, r *http.Request) {
	response, err := a.grpcApi.getAllChannelStoreData()
	if err != nil {
		a.logger.Error("Failed to get channels data: %v", err)
		http.Error(w, "Failed to get channels data", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	w.WriteHeader(http.StatusOK)
}

func (a *RESTApiService) GetUptime(w http.ResponseWriter, r *http.Request) {
}

// Initialize the router
func (a *RESTApiService) initializeRouter() *mux.Router {
	router := mux.NewRouter()

	router.Use(a.loggingMiddleware)

	// Add token middleware
	apiRouter := router.PathPrefix("/api/v1").Subrouter()

	// Define API endpoints
	apiRouter.HandleFunc("/log", a.LogMessage).Methods(http.MethodPost)
	apiRouter.HandleFunc("/docker/dns", a.UpdateDockerDNS).Methods(http.MethodPatch)
	apiRouter.HandleFunc("/docker/dns", a.GetDockerDNS).Methods(http.MethodGet)
	apiRouter.HandleFunc("/docker/creds-helper", a.GetDockerCredsHelper).Methods(http.MethodGet)
	apiRouter.HandleFunc("/ssh/channels", a.GetSSHChannels).Methods(http.MethodGet)
	apiRouter.HandleFunc("/tools/uptime", a.GetUptime).Methods(http.MethodGet)
	a.logRoutes(router)
	return router
}

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

		a.logger.Debug("Route: %s Methods: %v", path, methods)
		return nil
	})

	if err != nil {
		a.logger.Info("Error walking routes: %v", err)
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
			a.logger.Info("Context cancelled, stopping Unix socket server loop.")
			return
		default:
		}

		a.logger.Warn("Creating unix socket %s", a.unixSocketPath)
		unixListener, err := net.Listen("unix", a.unixSocketPath)
		if err != nil {
			a.logger.Error("Error creating Unix socket listener: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		err = os.Chown(a.unixSocketPath, a.user.Uid, a.user.Gid)
		if err != nil {
			a.logger.Error("Error changing ownership of Unix socket: %v", err)
			unixListener.Close()
			os.Remove(a.unixSocketPath)
			time.Sleep(5 * time.Second)
			continue
		}

		a.logger.Info("Unix socket server started at %s", a.unixSocketPath)

		server := &http.Server{
			Handler: router,
		}

		errCh := make(chan error, 1)

		go func() {
			errCh <- server.Serve(unixListener)
		}()

		select {
		case <-ctx.Done():
			a.logger.Info("Context cancelled. Shutting down Unix socket server...")
			server.Shutdown(context.Background())
			unixListener.Close()
			return
		case err := <-errCh:
			if err != nil && err != http.ErrServerClosed {
				a.logger.Error("Socket server error: %v", err)
			}
			a.logger.Warn("Unix socket server terminated. Restarting...")
			unixListener.Close()
			time.Sleep(5 * time.Second)
			continue
		}
	}
}
