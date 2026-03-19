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

	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/k8shelld/internal/apps"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/internal/utils"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"

	apiClient "github.com/k8shell-io/api-server/pkg/client"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const cleanupInterval = 1 * time.Minute // The interval for cleaning up the stores
const deleteDelay = 1 * time.Minute     // The delay after the channel was stopped before deleting an entry
const timeFormat = time.RFC3339         // The time format for the created and deleted fields

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
	Config              *config.Config              // The main configuration
	user                *models.User                // The user information loaded from the identity token
	logger              *zerolog.Logger             // The logger
	procWatcher         *system.ProcessWatcher      // The process watcher
	portForwardingRules []config.PortForwardingRule // The port forwarding rules that are allowed
	ExecStore           *sync.Map                   // The store for the exec data
	PortForwardStore    *sync.Map                   // The store for the port forwarding data
	SessionStore        *sync.Map                   // The store for the session data
	UnixSocketStore     *sync.Map                   // The store for the unix socket data
	apiClientx          *apiClient.Client           // The API client to communicate with the API server
	appManager          *apps.AppManager            // The app manager
	CommandService      *CommandServiceServer       // The command service
	sysInfo             *system.SystemInfo          // The system information
	jwtVerifier         *authz.JWTVerifier          // The JWT verifier for the identity token
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

// NewGRPCAPI creates a new GRPCApiService
func NewGRPCService(config *config.Config, user *models.User, jwtVerifier *authz.JWTVerifier,
	portForwardingRules []config.PortForwardingRule,
	procWatcher *system.ProcessWatcher, apiClient *apiClient.Client,
	appManager *apps.AppManager, sysInfo *system.SystemInfo) (*GRPCService, error) {

	logger := logger.NewLogger("grpc")

	return &GRPCService{
		logger:              logger,
		Config:              config,
		user:                user,
		portForwardingRules: portForwardingRules,
		procWatcher:         procWatcher,
		ExecStore:           &sync.Map{},
		PortForwardStore:    &sync.Map{},
		SessionStore:        &sync.Map{},
		UnixSocketStore:     &sync.Map{},
		apiClientx:          apiClient,
		appManager:          appManager,
		CommandService:      NewCommandServiceServer(),
		sysInfo:             sysInfo,
		jwtVerifier:         jwtVerifier,
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
		k8shelldpb.RegisterSystemServiceServer(s, NewSystemServiceServer(a))
		k8shelldpb.RegisterShellServiceServer(s, NewShellServiceServer(a))
		k8shelldpb.RegisterExecServiceServer(s, NewExecServiceServer(a))
		k8shelldpb.RegisterPortForwardServiceServer(s, NewPortForwardServiceServer(a))
		k8shelldpb.RegisterUnixSocketServiceServer(s, NewUnixSocketServiceServer(a))
		k8shelldpb.RegisterAppServiceServer(s, NewAppServiceServer(a.appManager))
		k8shelldpb.RegisterCommandServiceServer(s, a.CommandService)
		a.logger.Info().Msgf("GRPC services server registered")
		return nil
	}); err != nil {
		return fmt.Errorf("failed to register services: %v", err)
	}

	// cleanup goroutine
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.cleanupChannelStores()
			case <-ctx.Done():
				a.logger.Info().Msgf("Cleanup goroutine exiting")
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
			return nil, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
		}

		if tokenStr != s.user.GetUserToken() {
			return nil, status.Errorf(codes.PermissionDenied, "invalid token: caller token does not match workspace token")
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

// Cleanup the stores by removing the entries that were deleted more than deleteDelay ago
func (a *GRPCService) cleanupChannelStores() {
	a.cleanupChannelStore(a.ExecStore, func(v any) bool {
		data := v.(*ExecData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.PortForwardStore, func(v any) bool {
		data := v.(*PortForwardData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.SessionStore, func(v any) bool {
		data := v.(*SessionData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.UnixSocketStore, func(v any) bool {
		data := v.(*unixSocketData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

}

// Generic cleanup function for any store
func (a *GRPCService) cleanupChannelStore(store *sync.Map, shouldDelete func(any) bool) {
	store.Range(func(key, value any) bool {
		if shouldDelete(value) {
			store.Delete(key)
		}
		return true
	})
}

func (a *GRPCService) GetAllChannelStoreData() ([]StoreRecord, error) {
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
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getStatus(v.Deleted),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   fmt.Sprintf("cmd=%s, pid=%d", v.CmdShell, v.Pid),
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
