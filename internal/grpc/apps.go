package grpc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/k8shell-io/k8shelld/internal/apps"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/pkg/api"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AppServiceServer implements the gRPC server for application management.
type AppServiceServer struct {
	k8shelldpb.UnimplementedAppServiceServer

	appManager *apps.AppManager
	logger     *zerolog.Logger
}

// NewAppServiceServer creates a new AppServiceServer instance.
func NewAppServiceServer(appManager *apps.AppManager) *AppServiceServer {
	return &AppServiceServer{
		appManager: appManager,
		logger:     logger.NewLogger("grpc-apps"),
	}
}

// Helper function to convert application errors to gRPC errors
func (s *AppServiceServer) grpcError(err error) error {
	if errors.Is(err, apps.ErrAppNotFound) {
		return status.Errorf(codes.NotFound, "%s", err.Error())
	}
	if errors.Is(err, apps.ErrNoAppsConfigured) {
		return status.Errorf(codes.NotFound, "%s", err.Error())
	}
	return status.Errorf(codes.Internal, "%s", err.Error())
}

// ListApps lists the status of all applications.
func (s *AppServiceServer) ListApps(ctx context.Context,
	req *k8shelldpb.ListAppsRequest) (*k8shelldpb.ListAppsResponse, error) {
	if s.appManager == nil {
		return nil, status.Errorf(codes.NotFound, "app manager not available")
	}

	statuses, err := s.appManager.ListAppStatus(ctx)
	if err != nil {
		return nil, s.grpcError(err)
	}

	resp := &k8shelldpb.ListAppsResponse{
		Apps: make([]*k8shelldpb.AppStatus, 0, len(statuses)),
	}

	for _, st := range statuses {
		resp.Apps = append(resp.Apps, api.AppStatusToProto(&st))
	}

	return resp, nil
}

// StopApp stops the specified application by name.
func (s *AppServiceServer) InstallApp(ctx context.Context,
	req *k8shelldpb.InstallAppRequest) (*k8shelldpb.InstallAppResponse, error) {
	if s.appManager == nil {
		return nil, status.Errorf(codes.NotFound, "app manager not available")
	}
	name := req.GetName()
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing app name")
	}

	err := s.appManager.InstallAsync(ctx, name, req.GetForce())
	if err != nil {
		return nil, s.grpcError(err)
	}

	return &k8shelldpb.InstallAppResponse{}, nil
}

// StopApp stops the specified application by name.
func (s *AppServiceServer) StartApp(ctx context.Context,
	req *k8shelldpb.StartAppRequest) (*k8shelldpb.StartAppResponse, error) {
	if s.appManager == nil {
		return nil, status.Errorf(codes.NotFound, "app manager not available")
	}
	name := req.GetName()
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing app name")
	}

	err := s.appManager.Start(ctx, name)
	if err != nil {
		return nil, s.grpcError(err)
	}

	return &k8shelldpb.StartAppResponse{}, nil
}

// StopApp stops the specified application by name.
func (s *AppServiceServer) StopApp(ctx context.Context,
	req *k8shelldpb.StopAppRequest) (*k8shelldpb.StopAppResponse, error) {
	if s.appManager == nil {
		return nil, status.Errorf(codes.NotFound, "app manager not available")
	}
	name := req.GetName()
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing app name")
	}

	err := s.appManager.Stop(ctx, name)
	if err != nil {
		return nil, s.grpcError(err)
	}

	return &k8shelldpb.StopAppResponse{}, nil
}

// GetLogs retrieves the logs of the specified type (install/app) for the given app name.
func (s *AppServiceServer) GetLogs(ctx context.Context,
	req *k8shelldpb.GetLogsRequest) (*k8shelldpb.GetLogsResponse, error) {
	if s.appManager == nil {
		return nil, status.Errorf(codes.NotFound, "app manager not available")
	}
	name := req.GetName()
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing app name")
	}

	logType := api.LogTypeFromProto(req.GetType())
	logStr, err := s.appManager.GetLastLog(name, logType)
	if err != nil {
		return nil, s.grpcError(err)
	}

	return &k8shelldpb.GetLogsResponse{
		Log: logStr,
	}, nil
}

// GetLogsStream streams the logs of the specified type (install/app) for the given app name.
func (s *AppServiceServer) GetLogsStream(req *k8shelldpb.GetLogsStreamRequest,
	stream k8shelldpb.AppService_GetLogsStreamServer) error {

	if s.appManager == nil {
		return status.Errorf(codes.NotFound, "app manager not available")
	}
	name := req.GetName()
	if name == "" {
		return status.Errorf(codes.InvalidArgument, "missing app name")
	}

	logType := api.LogTypeFromProto(req.GetType())

	logPath, err := s.appManager.GetLastLogPath(name, logType)
	if err != nil {
		return s.grpcError(err)
	}
	if logPath == "" {
		return status.Errorf(codes.NotFound, "no %s logs found", logType)
	}

	sendWholeFile := func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to open %s log file: %v", logType, err)
		}
		defer f.Close()

		reader := bufio.NewReader(f)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				if line[len(line)-1] == '\n' {
					line = line[:len(line)-1]
				}
				if sendErr := stream.Send(&k8shelldpb.GetLogsStreamResponse{Line: line}); sendErr != nil {
					return status.Errorf(codes.Canceled, "client canceled")
				}
			}
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return status.Errorf(codes.Internal, "failed to read %s logs: %v", logType, err)
			}
		}
	}

	isActive := func() bool {
		if logType == "install" {
			return s.appManager.IsInstalling(name)
		}
		return s.appManager.IsRunning(name)
	}

	if !isActive() {
		return sendWholeFile(logPath)
	}

	f, err := os.Open(logPath)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to open %s log file: %v", logType, err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	ctx := stream.Context()

	for {
		select {
		case <-ctx.Done():
			return status.Errorf(codes.Canceled, "client canceled")
		default:
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				if line[len(line)-1] == '\n' {
					line = line[:len(line)-1]
				}
				if sendErr := stream.Send(&k8shelldpb.GetLogsStreamResponse{Line: line}); sendErr != nil {
					return status.Errorf(codes.Canceled, "client canceled")
				}
			}
			if err != nil {
				if err == io.EOF {
					if !isActive() {
						return nil
					}
					time.Sleep(200 * time.Millisecond)
					continue
				}
				return status.Errorf(codes.Internal, "failed to read %s logs: %v", logType, err)
			}
		}
	}
}
