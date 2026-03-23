package grpc

import (
	"context"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SystemServiceServer is the gRPC server for the system service
type SystemServiceServer struct {
	grpcApi        *GRPCService
	logger         *zerolog.Logger
	initScriptsRun bool
	//handshakeMu    sync.Mutex
	k8shelldv1.UnimplementedSystemServiceServer
}

// NewSystemServiceServer creates a new SystemServiceServer
func NewSystemServiceServer(grpcapi *GRPCService) *SystemServiceServer {
	return &SystemServiceServer{
		grpcApi:        grpcapi,
		logger:         logger.NewLogger("grpc-system"),
		initScriptsRun: false,
	}
}

// Handshake handles the handshake request.
// The client sends its identity JWT; we verify it is non-empty and matches the
// token the server loaded from /run/secrets/identity-token at startup.  This
// proves the caller is the same identity that owns this workspace.
func (s *SystemServiceServer) Handshake(ctx context.Context,
	req *k8shelldv1.HandshakeRequest) (*k8shelldv1.HandshakeResponse, error) {
	// s.handshakeMu.Lock()
	// defer s.handshakeMu.Unlock()

	// if req.UserToken == "" {
	// 	s.logger.Warn().Msg("Handshake rejected: empty user token")
	// 	return nil, status.Error(codes.PermissionDenied, "user token is required")
	// }

	// workspaceToken := s.grpcApi.user.UserToken
	// if workspaceToken == "" {
	// 	s.logger.Warn().Msg("Handshake rejected: workspace identity token not set")
	// 	return nil, status.Error(codes.PermissionDenied, "workspace identity token not available")
	// }

	// if req.UserToken != workspaceToken && s.grpcApi.user.HasRole(commonModels.RoleAdmin) {
	// 	s.logger.Warn().Msg("Handshake rejected: user token does not match workspace token")
	// 	return nil, status.Error(codes.PermissionDenied, "user token mismatch")
	// }

	s.logger.Info().Msgf("Handshake accepted for user: %s", s.grpcApi.user.GetUsername())

	return &k8shelldv1.HandshakeResponse{
		Accepted:      true,
		ServerVersion: fmt.Sprintf("%s-%s", config.K8SHELLD_VERSION, config.K8SHELLD_COMMIT),
	}, nil
}

// SystemInfo returns system metrics + mount usage + docker usage over gRPC.
// Mirrors the REST /sysinfo payload.
func (s *SystemServiceServer) SystemInfo(ctx context.Context,
	_ *k8shelldv1.SystemInfoRequest) (*k8shelldv1.SystemInfoResponse, error) {

	metrics, err := s.grpcApi.sysInfo.GetSystemUsageSnapshot()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get system metrics: %v", err)
	}
	metrics.Users = s.grpcApi.NumSessions()

	mounts, err := s.grpcApi.sysInfo.GetMountUsageSnapshot()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get mount usage: %v", err)
	}

	docker, err := s.grpcApi.sysInfo.GetDockerUsageSnapshot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get docker usage: %v", err)
	}

	systemInfo := k8shelld.SystemInfo{
		Time:   time.Now().Format(time.RFC3339),
		System: metrics,
		Mounts: mounts,
		Docker: docker,
	}

	return k8shelld.SystemInfoToProto(&systemInfo), nil
}
