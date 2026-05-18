package grpc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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

// Handshake validates version compatibility.
// Returns the server version and Accepted=true on success; returns a
// descriptive Message and Accepted=false (not a gRPC error) for version
// mismatches so the caller can surface a helpful message to the user.
func (s *SystemServiceServer) Handshake(ctx context.Context,
	req *k8shelldv1.HandshakeRequest) (*k8shelldv1.HandshakeResponse, error) {

	serverVersion := fmt.Sprintf("%s-%s", config.K8SHELLD_VERSION, config.K8SHELLD_COMMIT)

	if req.ClientVersion != "" {
		if !majorVersionsMatch(config.K8SHELLD_VERSION, req.ClientVersion) {
			msg := fmt.Sprintf(
				"client version %s is not compatible with server version %s (major version mismatch)",
				req.ClientVersion, config.K8SHELLD_VERSION,
			)
			s.logger.Warn().Msg("Handshake rejected: " + msg)
			return &k8shelldv1.HandshakeResponse{
				Accepted:      false,
				ServerVersion: serverVersion,
				Message:       msg,
			}, nil
		}
	}

	username := s.grpcApi.user.GetUsername()

	s.logger.Info().Msgf("Handshake accepted for user %s (client version: %s, server version: %s)",
		username, req.ClientVersion, serverVersion)

	return &k8shelldv1.HandshakeResponse{
		Accepted:      true,
		ServerVersion: serverVersion,
	}, nil
}

// majorVersionsMatch returns true when the major component of two semver
// strings (e.g. "1.2.3" or "1.2.3-abc") are equal.  If either string cannot
// be parsed the check is skipped and true is returned so that dev / snapshot
// builds ("0.0.0") are never incorrectly blocked.
func majorVersionsMatch(serverVer, clientVer string) bool {
	serverMajor, err := parseMajor(serverVer)
	if err != nil {
		return true
	}
	clientMajor, err := parseMajor(clientVer)
	if err != nil {
		return true
	}
	return serverMajor == clientMajor
}

func parseMajor(ver string) (int, error) {
	// Strip build metadata or pre-release suffixes after the first '-'.
	core := strings.SplitN(ver, "-", 2)[0]
	parts := strings.SplitN(core, ".", 2)
	return strconv.Atoi(parts[0])
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

	podman, err := s.grpcApi.sysInfo.GetDockerUsageSnapshot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get podman usage: %v", err)
	}

	systemInfo := k8shelld.SystemInfo{
		Time:   time.Now().Format(time.RFC3339),
		System: metrics,
		Mounts: mounts,
		Docker: podman,
	}

	return k8shelld.SystemInfoToProto(&systemInfo), nil
}
