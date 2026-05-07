// grpcapi.go, copyright 2025 the k8shell.io authors

// gRPC API service creates a gRPC server, registers the services, sets up the TLS configuration,
// and the interceptor. It uses the tokenAuthInterceptor to authenticate the client using the token
// in the metadata.

package grpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/gapi"
	commonmodels "github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/k8shelld/internal/apps"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/internal/utils"

	apiClient "github.com/k8shell-io/api-server/pkg/client"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const cleanupInterval = 1 * time.Minute // The interval for cleaning up the stores
const deleteDelay = 1 * time.Minute     // The delay after the stream was stopped before deleting an entry
const timeFormat = time.RFC3339         // The time format for the created and deleted fields
const sessionLockTTL = 30 * time.Second // How long an AcquireSession lock is held before auto-release

type StoreRecord struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Created  string `json:"created"`
	Deleted  string `json:"deleted"`
	Status   string `json:"status"`
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
	Params   string `json:"params"`
}

// GRPCApiService is the main service that handles the gRPC API
type GRPCService struct {
	Config             *config.Config          // The main configuration
	blueprint          *commonmodels.Blueprint // The workspace blueprint
	user               *models.User            // The user information loaded from the identity token
	logger             *zerolog.Logger         // The logger
	procWatcher        *system.ProcessWatcher  // The process watcher
	ExecStore          *sync.Map               // The store for the exec data
	PortForwardStore   *sync.Map               // The store for the port forwarding data
	SessionStore       *sync.Map               // The store for the session data
	UnixSocketStore    *sync.Map               // The store for the unix socket data
	apiClientx         *apiClient.Client       // The API client to communicate with the API server
	appManager         *apps.AppManager        // The app manager
	CommandService     *CommandServiceServer   // The command service
	sysInfo            *system.SystemInfo      // The system information
	jwtVerifier        *authz.JWTVerifier      // The JWT verifier for the identity token
	updateToken        func(string) error      // Called on each token refresh
	detachedSessionTTL time.Duration           // max TTL for sessions with no client; 0 = no GC
	allowSessionDetach bool                    // whether clients may detach/attach PTY sessions
	allowUnlimitedTTL  bool                    // whether clients may request ttl=0 (never expire)
	SessionLockStore   *sync.Map               // stores *sessionLock keyed by lock ID
	acquireMu          sync.Mutex              // serialises AcquireSession scan-then-store
}

// Helper function to get the deletion date as a string or empty if not set
func getDeletedDate(deleted time.Time) string {
	if deleted.IsZero() {
		return ""
	}
	return deleted.Format(timeFormat)
}

// Helper function to determine the status
func getStatus(deleted time.Time) string {
	if deleted.IsZero() {
		return "ACTIVE"
	}
	return "STOPPED"
}

// getSessionStatus returns the status string for a shell session, distinguishing
// detached sessions (process alive but no client attached) from active ones.
func getSessionStatus(session *SessionData) string {
	if !session.Deleted.IsZero() {
		return "STOPPED"
	}
	session.mu.Lock()
	detachedAt := session.DetachedAt
	session.mu.Unlock()
	if !detachedAt.IsZero() {
		return "DETACHED"
	}
	return "ACTIVE"
}

// NewGRPCAPI creates a new GRPCApiService
func NewGRPCService(config *config.Config, blueprint *commonmodels.Blueprint, user *models.User,
	jwtVerifier *authz.JWTVerifier, procWatcher *system.ProcessWatcher, apiClient *apiClient.Client,
	appManager *apps.AppManager, sysInfo *system.SystemInfo, updateToken func(string) error) (*GRPCService, error) {

	logger := logger.NewLogger("grpc")

	detachedTTL := defaultDetachedSessionTTL
	if cfg := config.Shells.DetachedTTL; cfg != "" {
		if d, err := time.ParseDuration(cfg); err != nil {
			return nil, fmt.Errorf("invalid shells.detachedTTL %q: %w", cfg, err)
		} else if d < 0 {
			return nil, fmt.Errorf("shells.detachedTTL must not be negative")
		} else {
			detachedTTL = d
		}
	}

	return &GRPCService{
		logger:             logger,
		Config:             config,
		blueprint:          blueprint,
		user:               user,
		procWatcher:        procWatcher,
		ExecStore:          &sync.Map{},
		PortForwardStore:   &sync.Map{},
		SessionStore:       &sync.Map{},
		UnixSocketStore:    &sync.Map{},
		apiClientx:         apiClient,
		appManager:         appManager,
		CommandService:     NewCommandServiceServer(),
		sysInfo:            sysInfo,
		jwtVerifier:        jwtVerifier,
		updateToken:        updateToken,
		detachedSessionTTL: detachedTTL,
		allowSessionDetach: config.Shells.AllowSessionDetach,
		allowUnlimitedTTL:  config.Shells.AllowUnlimittedTTL,
		SessionLockStore:   &sync.Map{},
	}, nil
}

// NumSessions returns the number of active sessions
func (a *GRPCService) NumSessions() uint32 {
	var sessions uint32 = 0
	a.SessionStore.Range(func(key, value any) bool {
		record, ok := value.(*SessionData)
		if ok && record.Deleted.UTC().IsZero() {
			sessions += 1
		}
		return true
	})
	return sessions
}

// Serve starts the gRPC server and registers the services.
// It also sets up the TLS configuration and the interceptor.
func (a *GRPCService) Serve(ctx context.Context) error {

	// create gRPC server, always stop forcibly
	// to avoid hanging connections on existing sessions during shutdown
	server, err := gapi.NewServer(&a.Config.System.GrpcConfig, false)
	if err != nil {
		return fmt.Errorf("failed to create gRPC server: %v", err)
	}

	server.AddInterceptor(a.callerValidationInterceptor())

	if err := server.RegisterService(func(s *grpc.Server) error {
		k8shelldv1.RegisterSystemServiceServer(s, NewSystemServiceServer(a))
		k8shelldv1.RegisterSshServiceServer(s, NewSshServiceServer(a))
		k8shelldv1.RegisterAppServiceServer(s, NewAppServiceServer(a.appManager))
		k8shelldv1.RegisterCommandServiceServer(s, a.CommandService)
		a.logger.Info().Msgf("GRPC services server registered")
		return nil
	}); err != nil {
		return fmt.Errorf("failed to register services: %v", err)
	}

	// cleanup goroutine — removes completed stream store entries after the delete delay
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.cleanupStreamStores()
				a.cleanupExpiredLocks()
			case <-ctx.Done():
				a.logger.Info().Msgf("Cleanup goroutine exiting")
				return
			}
		}
	}()

	// detachable session GC — terminates idle detachable sessions that have
	// exceeded their timeout, and removes sessions whose shell process has exited
	go func() {
		ticker := time.NewTicker(detachableGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.gcDetachedSessions()
			case <-ctx.Done():
				return
			}
		}
	}()

	errChan := make(chan error, 1)
	go func() {
		if err := server.Start(); err != nil && err != grpc.ErrServerStopped {
			errChan <- fmt.Errorf("gRPC server error: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
		a.logger.Info().Msg("Shutting down gRPC server")
		server.Stop()
		return nil
	case err := <-errChan:
		return err
	}
}

func (s *GRPCService) callerValidationInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "missing metadata")
		}

		data := md.Get("token")
		if len(data) == 0 {
			return nil, status.Errorf(codes.InvalidArgument, "missing token in metadata")
		}

		tokenStr := data[0]
		if tokenStr == "" {
			return nil, status.Errorf(codes.InvalidArgument, "empty token in metadata")
		}

		_, err = s.jwtVerifier.VerifyToken(tokenStr)
		if err != nil {
			return nil, status.Errorf(codes.PermissionDenied, "invalid token: %v", err)
		}

		if !s.user.TokenEqual(tokenStr) {
			return nil, status.Errorf(codes.PermissionDenied, "invalid token: caller token does not match workspace token")
		}

		if s.updateToken != nil {
			if err := s.updateToken(tokenStr); err != nil {
				return nil, status.Errorf(codes.PermissionDenied, "token validation failed: %v", err)
			}
		}

		return handler(ctx, req)
	}
}

// resolveShellUser determines which OS user the shell session should run as.
// Priority: explicit "root" (requires sudo) > named user lookup > default user.
func (s *GRPCService) resolveShellUser(reqUser string, callerUser *models.User) (models.ShellUser, error) {
	if reqUser == "root" {
		if callerUser.SudoEnabled() {
			return models.ShellUser{Username: "root", UID: 0, GID: 0, HomeDir: "/root"}, nil
		}
		return models.ShellUser{}, fmt.Errorf("user %s does not have sudo privileges", callerUser.GetUsername())
	}

	if reqUser != "" && reqUser != callerUser.GetUsername() {
		u := system.UserExists(reqUser)
		if u == nil {
			return models.ShellUser{}, fmt.Errorf("requested user %s not found", reqUser)
		}
		uid, err := utils.ParseUint32(u.Uid)
		if err != nil {
			return models.ShellUser{}, fmt.Errorf("failed to parse UID for user %s: %v", u.Username, err)
		}
		gid, err := utils.ParseUint32(u.Gid)
		if err != nil {
			return models.ShellUser{}, fmt.Errorf("failed to parse GID for user %s: %v", u.Username, err)
		}
		return models.ShellUser{Username: u.Username, UID: uid, GID: gid, HomeDir: u.HomeDir}, nil
	}

	return models.NewShellUser(callerUser), nil
}

// cleanupStreamStores removes stream store entries that were deleted more than deleteDelay ago.
func (a *GRPCService) cleanupStreamStores() {
	a.cleanupStreamStore(a.ExecStore, func(v any) bool {
		data := v.(*ExecData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupStreamStore(a.PortForwardStore, func(v any) bool {
		data := v.(*PortForwardData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupStreamStore(a.SessionStore, func(v any) bool {
		data := v.(*SessionData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupStreamStore(a.UnixSocketStore, func(v any) bool {
		data := v.(*unixSocketData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

}

// cleanupStreamStore is a generic cleanup helper for any stream store.
func (a *GRPCService) cleanupStreamStore(store *sync.Map, shouldDelete func(any) bool) {
	store.Range(func(key, value any) bool {
		if shouldDelete(value) {
			store.Delete(key)
		}
		return true
	})
}

// sessionTTLRemaining returns how long a detached session has before GC expires it.
// Returns "-" when the session is not detached or already stopped.
// Returns "∞" when the effective TTL is zero (never expire).
func (a *GRPCService) sessionTTLRemaining(session *SessionData) string {
	if !session.Deleted.IsZero() {
		return "-"
	}
	session.mu.Lock()
	detachedAt := session.DetachedAt
	perSession := session.DetachTTL
	session.mu.Unlock()

	if detachedAt.IsZero() {
		return "-"
	}

	var effectiveTTL time.Duration
	if perSession != nil {
		effectiveTTL = *perSession
	} else {
		effectiveTTL = a.detachedSessionTTL
	}

	if effectiveTTL == 0 {
		return "∞"
	}

	remaining := time.Until(detachedAt.Add(effectiveTTL)).Round(time.Second)
	if remaining <= 0 {
		return "0s"
	}
	return remaining.String()
}

func (a *GRPCService) GetAllStreamData() ([]StoreRecord, error) {
	var result []StoreRecord

	// Helper function to process each store
	processStore := func(storeName string, store *sync.Map) {
		store.Range(func(key, value any) bool {
			switch v := value.(type) {
			case *ExecData:
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getStatus(v.Deleted),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   fmt.Sprintf("cmd=%s", v.Command),
				}
				result = append(result, record)
			case *PortForwardData:
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getStatus(v.Deleted),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   fmt.Sprintf("dest=%s, port=%d", v.Destination, v.Port),
				}
				result = append(result, record)
			case *SessionData:
				params := fmt.Sprintf("cmd=%s, pid=%d", v.CmdShell, v.Pid)
				if ttl := a.sessionTTLRemaining(v); ttl != "-" {
					params += fmt.Sprintf(", ttl=%s", ttl)
				}
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getSessionStatus(v),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   params,
				}
				result = append(result, record)
			case *unixSocketData:
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getStatus(v.Deleted),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   fmt.Sprintf("mode=%s, socket=%s", v.Mode, v.socketPath),
				}
				result = append(result, record)
			}
			return true
		})
	}

	processStore("exec", a.ExecStore)
	processStore("port-forward", a.PortForwardStore)
	processStore("shell", a.SessionStore)
	processStore("unix-socket", a.UnixSocketStore)

	return result, nil
}
