// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package grpc

import (
	"context"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"google.golang.org/grpc"
)

// Ensure SshServiceServer satisfies the interface at compile time.
var _ k8shelldv1.SshServiceServer = (*SshServiceServer)(nil)

// SshServiceServer implements k8shelldv1.SshServiceServer by composing the
// individual service implementations.
type SshServiceServer struct {
	shell      *ShellHandler
	exec       *ExecHandler
	portfwd    *PortForwardHandler
	unixsocket *UnixSocketHandler
	k8shelldv1.UnimplementedSshServiceServer
}

// NewSshServiceServer creates a new SshServiceServer.
func NewSshServiceServer(grpcapi *GRPCService) *SshServiceServer {
	return &SshServiceServer{
		shell:      newShellHandler(grpcapi),
		exec:       newExecHandler(grpcapi),
		portfwd:    newPortForwardHandler(grpcapi),
		unixsocket: newUnixSocketHandler(grpcapi),
	}
}

func (s *SshServiceServer) Shell(stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse]) error {
	return s.shell.Shell(stream)
}

func (s *SshServiceServer) ResizeTerminal(ctx context.Context, req *k8shelldv1.ResizeTerminalRequest) (*k8shelldv1.ResizeTerminalResponse, error) {
	return s.shell.ResizeTerminal(ctx, req)
}

func (s *SshServiceServer) GetCWD(ctx context.Context, req *k8shelldv1.GetCWDRequest) (*k8shelldv1.GetCWDResponse, error) {
	return s.shell.GetCWD(ctx, req)
}

func (s *SshServiceServer) AcquireSession(ctx context.Context, req *k8shelldv1.AcquireSessionRequest) (*k8shelldv1.AcquireSessionResponse, error) {
	return s.shell.AcquireSession(ctx, req)
}

func (s *SshServiceServer) ListSessions(ctx context.Context, req *k8shelldv1.ListSessionsRequest) (*k8shelldv1.ListSessionsResponse, error) {
	return s.shell.ListSessions(ctx, req)
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
