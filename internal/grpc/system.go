package grpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/pkg/api"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SystemServiceServer is the gRPC server for the system service
type SystemServiceServer struct {
	grpcApi        *GRPCService
	logger         *zerolog.Logger
	initScriptsRun bool
	handshakeMu    sync.Mutex
	k8shelldpb.UnimplementedSystemServiceServer
}

// NewSystemServiceServer creates a new SystemServiceServer
func NewSystemServiceServer(grpcapi *GRPCService) *SystemServiceServer {
	return &SystemServiceServer{
		grpcApi:        grpcapi,
		logger:         logger.NewLogger("grpc-system"),
		initScriptsRun: false,
	}
}

// Handshake handles the handshake request
func (s *SystemServiceServer) Handshake(ctx context.Context,
	req *k8shelldpb.HandshakeRequest) (*k8shelldpb.HandshakeResponse, error) {
	s.handshakeMu.Lock()
	defer s.handshakeMu.Unlock()

	s.logger.Info().Msgf("Received handshake from user: %s, uid: %d, gid: %d",
		req.User.Username, req.User.Uid, req.User.Gid)

	if s.grpcApi.Config.User.Username != req.User.Username {
		return nil, status.Error(codes.PermissionDenied, "user name mismatch")
	}

	if s.grpcApi.Config.User.Uid != req.User.Uid {
		return nil, status.Error(codes.PermissionDenied, "user uid mismatch")
	}

	if s.grpcApi.Config.User.Gid != req.User.Gid {
		return nil, status.Error(codes.PermissionDenied, "user gid mismatch")
	}

	if req.User.UserToken == "" {
		s.logger.Warn().Msg("Empty user token received in handshake")
	} else {
		var tokenPreview string
		if len(req.User.UserToken) >= 4 {
			tokenPreview = req.User.UserToken[:4]
		} else {
			tokenPreview = "****"
		}

		s.logger.Debug().Msgf("User token received in handshake: token=***%s", tokenPreview)
		s.grpcApi.Config.User.UserToken = req.User.UserToken
		if s.grpcApi.apiClientx != nil {
			s.grpcApi.apiClientx.UpdateToken(req.User.UserToken)
		}
	}

	s.logger.Info().Msg("Handshake successful")

	return &k8shelldpb.HandshakeResponse{
		Accepted:      true,
		ServerVersion: fmt.Sprintf("%s-%s", config.K8SHELLD_VERSION, config.K8SHELLD_COMMIT),
	}, nil
}

// SystemInfo returns system metrics + mount usage + docker usage over gRPC.
// Mirrors the REST /sysinfo payload.
func (s *SystemServiceServer) SystemInfo(ctx context.Context,
	_ *k8shelldpb.SystemInfoRequest) (*k8shelldpb.SystemInfoResponse, error) {

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

	systemInfo := api.SystemInfo{
		Time:   time.Now().Format(time.RFC3339),
		System: metrics,
		Mounts: mounts,
		Docker: docker,
	}

	return api.SystemInfoToProto(&systemInfo), nil
}
