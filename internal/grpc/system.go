// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

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
	"gopkg.in/yaml.v3"
)

// logStreamPollInterval is how often GetLogsStream polls the in-memory log
// store for new entries while following (mirrors the REST /logs handler).
const logStreamPollInterval = 100 * time.Millisecond

// defaultLogPageLimit is the page size GetLogsPage falls back to when the
// caller doesn't specify one (mirrors the REST /logs handler's default).
const defaultLogPageLimit = 100

// SystemServiceServer is the gRPC server for the system service
type SystemServiceServer struct {
	grpcApi        *GRPCService
	logger         *zerolog.Logger
	initScriptsRun bool
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

// SystemInfoHistory returns historical system/mount/docker usage samples
// for charting. See the proto comment on SystemInfoHistoryRequest for the
// precedence of range vs from/to and how step coarsening works.
func (s *SystemServiceServer) SystemInfoHistory(_ context.Context,
	req *k8shelldv1.SystemInfoHistoryRequest) (*k8shelldv1.SystemInfoHistoryResponse, error) {

	query := k8shelld.SystemInfoHistoryQuery{
		From:  req.GetFrom(),
		To:    req.GetTo(),
		Range: req.GetRange(),
		Step:  req.GetStep(),
	}

	hist, err := s.grpcApi.sysInfo.GetSystemInfoHistory(query)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid system info history request: %v", err)
	}

	return k8shelld.SystemInfoHistoryToProto(hist), nil
}

// GetLogsStream streams k8shelld daemon logs (the same logs shown by
// `kbox logs`). With Follow=false it sends the currently buffered entries
// and closes the stream; with Follow=true it keeps streaming new entries as
// they are produced until the client cancels.
func (s *SystemServiceServer) GetLogsStream(req *k8shelldv1.SystemLogsStreamRequest,
	stream k8shelldv1.SystemService_GetLogsStreamServer) error {

	component := req.GetComponent()
	level := k8shelld.LogLevelFromProto(req.GetLevel())
	follow := req.GetFollow()

	send := func(entries []logger.LogEntry) error {
		for _, entry := range entries {
			if sendErr := stream.Send(logEntryToProto(entry)); sendErr != nil {
				return status.Errorf(codes.Canceled, "client canceled")
			}
		}
		return nil
	}

	var backlog []logger.LogEntry
	var sinceID int64
	if n := req.GetLastN(); n > 0 {
		backlog, _ = logger.GetLogsBefore(0, int(n), component, level)
		if len(backlog) > 0 {
			sinceID = backlog[len(backlog)-1].ID
		}
	} else {
		backlog, sinceID = logger.GetLogsSince(0, component, level)
	}
	if err := send(backlog); err != nil {
		return err
	}

	if !follow {
		return nil
	}

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return status.Errorf(codes.Canceled, "client canceled")
		default:
			entries, newSinceID := logger.GetLogsSince(sinceID, component, level)
			if err := send(entries); err != nil {
				return err
			}
			sinceID = newSinceID

			time.Sleep(logStreamPollInterval)
		}
	}
}

// GetLogsPage returns one page of k8shelld daemon logs strictly older than
// the requested BeforeId, for "load more" / infinite-scroll style backward
// pagination independent of GetLogsStream's live tail.
func (s *SystemServiceServer) GetLogsPage(ctx context.Context,
	req *k8shelldv1.GetLogsPageRequest) (*k8shelldv1.GetLogsPageResponse, error) {

	if req.GetBeforeId() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "before_id must not be negative")
	}
	if req.GetLimit() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "limit must not be negative")
	}

	limit := int(req.GetLimit())
	if limit == 0 {
		limit = defaultLogPageLimit
	}

	component := req.GetComponent()
	level := k8shelld.LogLevelFromProto(req.GetLevel())

	entries, hasMore := logger.GetLogsBefore(req.GetBeforeId(), limit, component, level)

	resp := &k8shelldv1.GetLogsPageResponse{
		Entries: make([]*k8shelldv1.SystemLogsStreamResponse, 0, len(entries)),
		HasMore: hasMore,
	}
	for _, entry := range entries {
		resp.Entries = append(resp.Entries, logEntryToProto(entry))
	}
	return resp, nil
}

// GetBlueprint returns the raw blueprint YAML content this workspace was
// created from.
func (s *SystemServiceServer) GetBlueprint(_ context.Context,
	_ *k8shelldv1.GetBlueprintRequest) (*k8shelldv1.GetBlueprintResponse, error) {

	if s.grpcApi.blueprint == nil {
		return nil, status.Errorf(codes.NotFound, "blueprint not available")
	}

	raw, err := yaml.Marshal(s.grpcApi.blueprint)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal blueprint: %v", err)
	}

	return &k8shelldv1.GetBlueprintResponse{Blueprint: raw}, nil
}

func logEntryToProto(entry logger.LogEntry) *k8shelldv1.SystemLogsStreamResponse {
	return &k8shelldv1.SystemLogsStreamResponse{
		Id:        entry.ID,
		Time:      entry.Timestamp,
		Component: entry.Component,
		Level:     entry.Level,
		Message:   entry.Message,
	}
}
