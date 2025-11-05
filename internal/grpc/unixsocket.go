package grpc

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
	"github.com/rs/zerolog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type unixSocketData struct {
	Id         string
	socketPath string
	listener   *net.UnixListener
	mu         sync.Mutex
	conn       net.Conn
	Created    time.Time
	Deleted    time.Time
	BytesIn    uint64
	BytesOut   uint64
}

// UnixSocketServiceServer is the service that handles the shell GRPC service server
type UnixSocketServiceServer struct {
	grpcApi *GRPCService
	logger  *zerolog.Logger
	k8shelldpb.UnimplementedUnixSocketServiceServer
}

// NewUnixSocketServiceServer creates a new UnixSocketServiceServer
func NewUnixSocketServiceServer(grpcapi *GRPCService) *UnixSocketServiceServer {
	return &UnixSocketServiceServer{
		grpcApi: grpcapi,
		logger:  logger.NewLogger("grpc-unixsocket"),
	}
}

// Get the unix socket ID from the gRPC metadata "unixsocket-id"
func (s *UnixSocketServiceServer) GetUnixSocketID(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "missing metadata")
	}

	data := md.Get("unixsocket-id")
	if len(data) == 0 {
		return "", status.Errorf(codes.InvalidArgument, "missing unixsocket-id")
	}

	return data[0], nil
}

// Get the unix socket data from the store. It uses the unix socket ID retrieved from the metadata
func (s *UnixSocketServiceServer) GetUnixSocketData(ctx context.Context) (*SessionData, error) {
	sid, err := s.GetUnixSocketID(ctx)
	if err != nil {
		return nil, err
	}
	value, ok := s.grpcApi.UnixSocketStore.Load(sid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "unixsocket-id %s not found", sid)
	}
	return value.(*SessionData), nil
}

func (s *UnixSocketServiceServer) UnixSocket(stream k8shelldpb.UnixSocketService_UnixSocketServer) error {
	uxid, err := s.GetUnixSocketID(stream.Context())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to get unixsocket-id: %v", err)
	}

	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive request: %v", err)
	}

	start, ok := req.Request.(*k8shelldpb.UnixSocketRequest_StartRequest)
	if !ok {
		return status.Errorf(codes.InvalidArgument,
			"invalid unix socket request, expected unix socket start request: %v", req)
	}

	switch start.StartRequest.Mode {
	case k8shelldpb.UnixSocketMode_UNIX_SOCKET_MODE_LISTEN:
		return s.startListenerAndBridge(uxid, start.StartRequest.SocketPath, stream)
	case k8shelldpb.UnixSocketMode_UNIX_SOCKET_MODE_DIAL:
		return s.dialAndBridge(uxid, start.StartRequest.SocketPath, stream)
	default:
		return status.Errorf(codes.InvalidArgument, "invalid unix socket mode")
	}
}

// startListenerAndBridge starts a Unix socket listener and bridges gRPC <-> conn
func (s *UnixSocketServiceServer) startListenerAndBridge(uxid, socketPath string,
	stream k8shelldpb.UnixSocketService_UnixSocketServer) error {

	unixsocket := &unixSocketData{
		Id:         uxid,
		socketPath: socketPath,
		Created:    time.Now(),
	}

	if _, err := os.Lstat(unixsocket.socketPath); err == nil {
		_ = os.Remove(unixsocket.socketPath)
	}

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: unixsocket.socketPath, Net: "unix"})
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create Unix socket listener: %v", err)
	}
	unixsocket.listener = l

	if err := os.Chown(unixsocket.socketPath, s.grpcApi.user.Gid, s.grpcApi.user.Gid); err != nil {
		return status.Errorf(codes.Internal, "failed to chown socket: %v", err)
	}
	if err := os.Chmod(unixsocket.socketPath, 0700); err != nil {
		return status.Errorf(codes.Internal, "failed to chmod socket: %v", err)
	}

	s.logger.Info().Msgf("unix-socket listener started, id=%s, path=%s", unixsocket.Id, unixsocket.socketPath)
	s.grpcApi.UnixSocketStore.Store(unixsocket.Id, unixsocket)
	defer func() {
		unixsocket.Deleted = time.Now()
		_ = unixsocket.listener.Close()
		s.logger.Info().Msgf("Unix socket stream ended, id=%s, path=%s", unixsocket.Id, unixsocket.socketPath)
	}()

	return s.communicate(unixsocket, stream)
}

// dialAndBridge dials a Unix socket and bridges gRPC <-> conn
func (s *UnixSocketServiceServer) dialAndBridge(uxid, socketPath string,
	stream k8shelldpb.UnixSocketService_UnixSocketServer) error {

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to dial unix socket %s: %v", socketPath, err)
	}
	defer conn.Close()

	s.logger.Info().Msgf("unix-socket dial connected, id=%s, target=%s", uxid, socketPath)

	errCh := make(chan error, 2)

	// gRPC -> Unix
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("recv error: %w", err)
				}
				return
			}
			data := req.GetData()
			if len(data) == 0 {
				continue
			}
			if _, werr := conn.Write(data); werr != nil {
				errCh <- fmt.Errorf("write to unix: %w", werr)
				return
			}
		}
	}()

	// Unix -> gRPC
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := conn.Read(buf)
			if rerr != nil {
				if rerr == io.EOF {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("read from unix: %w", rerr)
				}
				return
			}
			if n == 0 {
				continue
			}
			if serr := stream.Send(&k8shelldpb.UnixSocketResponse{Data: buf[:n]}); serr != nil {
				errCh <- fmt.Errorf("send to stream: %w", serr)
				return
			}
		}
	}()

	// Wait for either side to end
	if err := <-errCh; err != nil {
		s.logger.Error().Msgf("unix-socket dial bridge error: %v", err)
		return status.Errorf(codes.Internal, "unix-socket dial bridge error: %v", err)
	}
	return nil
}

func (s *UnixSocketServiceServer) communicate(unixsocket *unixSocketData,
	stream k8shelldpb.UnixSocketService_UnixSocketServer) error {
	buf := make([]byte, 1024)
	stop := make(chan struct{})
	defer close(stop)

	// Goroutine to read from unixConn and send to gRPC stream
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				// Accept the connection from the client
				s.logger.Info().Msgf("Waiting for the client connection")
				conn, err := unixsocket.listener.Accept()
				if err != nil {
					if err == io.EOF {
						s.logger.Info().Msg("Unix listener closed")
					} else {
						s.logger.Error().Msgf("Failed to accept connection: %v", err)
					}
					break
				}

				s.logger.Info().Msg("Client connected")

				for {
					unixsocket.mu.Lock()
					unixsocket.conn = conn
					unixsocket.mu.Unlock()

					if conn == nil {
						break
					}

					n, err := conn.Read(buf)
					if err != nil {
						if err == io.EOF {
							s.logger.Info().Msg("Client connection closed")
						} else {
							s.logger.Error().Msgf("Error reading from unix socket: %v", err)
						}
						break
					} else {
						// Send data to the gRPC stream
						if err := stream.Send(&k8shelldpb.UnixSocketResponse{
							Data: buf[:n],
						}); err != nil {
							s.logger.Error().Msgf("Failed to send data to the client: %v", err)
							break
						}
						unixsocket.BytesOut += uint64(n)
					}
				}

				// Close the connection
				unixsocket.mu.Lock()
				unixsocket.conn.Close()
				unixsocket.conn = nil
				unixsocket.mu.Unlock()
			}
		}
	}()

	// Main loop to read from gRPC stream and write to unixConn
	for {
		req, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				s.logger.Info().Msg("Client closed the stream")
			} else {
				s.logger.Error().Msgf("Failed to receive: %v", err)
			}
			break
		}

		unixsocket.mu.Lock()
		conn := unixsocket.conn
		unixsocket.mu.Unlock()

		if conn != nil {
			data := req.GetData()
			if _, err := conn.Write(data); err != nil {
				s.logger.Error().Msgf("Failed to write data to the unix socket: %v", err)
				break
			}
			unixsocket.BytesIn += uint64(len(data))
		}
	}

	s.logger.Info().Msg("Communication ended")
	unixsocket.listener.Close()
	return nil
}
