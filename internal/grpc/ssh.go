package grpc

import (
	"context"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"google.golang.org/grpc"
)

// SshServiceServer implements k8shelldv1.SshServiceServer by composing the
// individual service implementations.
type SshServiceServer struct {
	shell      *ShellServiceServer
	exec       *ExecServiceServer
	portfwd    *PortForwardServiceServer
	unixsocket *UnixSocketServiceServer
	k8shelldv1.UnimplementedSshServiceServer
}

// NewSshServiceServer creates a new SshServiceServer.
func NewSshServiceServer(grpcapi *GRPCService) *SshServiceServer {
	return &SshServiceServer{
		shell:      NewShellServiceServer(grpcapi),
		exec:       NewExecServiceServer(grpcapi),
		portfwd:    NewPortForwardServiceServer(grpcapi),
		unixsocket: NewUnixSocketServiceServer(grpcapi),
	}
}

func (s *SshServiceServer) Shell(stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse]) error {
	return s.shell.Shell(stream)
}

func (s *SshServiceServer) ResizeTerminal(ctx context.Context, req *k8shelldv1.ResizeTerminalRequest) (*k8shelldv1.ResizeTerminalResponse, error) {
	return s.shell.ResizeTerminal(ctx, req)
}

func (s *SshServiceServer) WatchShell(req *k8shelldv1.WatchShellRequest, stream grpc.ServerStreamingServer[k8shelldv1.WatchShellEvent]) error {
	return s.shell.WatchShell(req, stream)
}

func (s *SshServiceServer) Exec(stream grpc.BidiStreamingServer[k8shelldv1.ExecRequest, k8shelldv1.ExecResponse]) error {
	return s.exec.Exec(stream)
}

func (s *SshServiceServer) PortForward(stream grpc.BidiStreamingServer[k8shelldv1.PortForwardRequest, k8shelldv1.PortForwardResponse]) error {
	return s.portfwd.PortForward(stream)
}

func (s *SshServiceServer) UnixSocket(stream grpc.BidiStreamingServer[k8shelldv1.UnixSocketRequest, k8shelldv1.UnixSocketResponse]) error {
	return s.unixsocket.UnixSocket(stream)
}
