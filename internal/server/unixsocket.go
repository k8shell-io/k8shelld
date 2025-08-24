package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/k8shell-io/k8shelld/internal/log"
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

// Get the port-forward ID from the gRPC metadata "portforward-id"
func (s *RemoteOSServiceServer) GetUnixSocketID(ctx context.Context) (string, error) {
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

// Get the port-forward data from the store. It uses the port-forward ID retrieved from the metadata
func (s *RemoteOSServiceServer) GetUnixSocketData(ctx context.Context) (*SessionData, error) {
	sid, err := s.GetUnixSocketID(ctx)
	if err != nil {
		return nil, err
	}
	value, ok := s.grpcApi.unixSocketStore.Load(sid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "unixsocket-id %s not found", sid)
	}
	return value.(*SessionData), nil
}

func (s *RemoteOSServiceServer) UnixSocket(stream k8shelldpb.RemoteOSService_UnixSocketServer) error {
	logger := log.NewLogger("grpc-unixsocket")
	uxid, err := s.GetUnixSocketID(stream.Context())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to get unixsocket-id: %v", err)
	}

	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive request: %v", err)
	}

	shellReq, ok := req.Request.(*k8shelldpb.UnixSocketRequest_StartRequest)
	if !ok {
		return status.Errorf(codes.InvalidArgument,
			"invalid unix socket request, expected unix socket start request: %v", req)
	}

	unixsocket := &unixSocketData{
		Id:         uxid,
		socketPath: shellReq.StartRequest.SocketPath,
		Created:    time.Now(),
		Deleted:    time.Time{},
		BytesIn:    0,
		BytesOut:   0,
	}

	unixsocket.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: unixsocket.socketPath, Net: "unix"})
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create Unix socket listener: %v", err)
	}

	// // Set the ownership of the Unix socket
	if err := os.Chown(unixsocket.socketPath, s.grpcApi.user.Gid, s.grpcApi.user.Gid); err != nil {
		return fmt.Errorf("failed to change ownership of Unix socket: %v", err)
	}

	// // Set the permissions of the Unix socket
	if err := os.Chmod(unixsocket.socketPath, 0700); err != nil {
		return fmt.Errorf("failed to change permissions of Unix socket: %v", err)
	}

	logger.Info().Msgf("unix-socket listener started, id=%s, path=%s", unixsocket.Id, unixsocket.socketPath)

	s.grpcApi.unixSocketStore.Store(unixsocket.Id, unixsocket)
	s.communicate(logger, unixsocket, stream)
	unixsocket.Deleted = time.Now()

	logger.Info().Msgf("Unix socket stream ended, id=%s, path=%s", unixsocket.Id, unixsocket.socketPath)
	return nil
}

func (s *RemoteOSServiceServer) communicate(logger *zerolog.Logger, unixsocket *unixSocketData, stream k8shelldpb.RemoteOSService_UnixSocketServer) error {
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
				logger.Info().Msgf("Waiting for the client connection")
				conn, err := unixsocket.listener.Accept()
				if err != nil {
					if err == io.EOF {
						logger.Info().Msg("Unix listener closed")
					} else {
						logger.Error().Msgf("Failed to accept connection: %v", err)
					}
					s.sendUnixSocketTerminate(stream)
					break
				}

				logger.Info().Msg("Client connected")

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
							logger.Info().Msg("Client connection closed")
						} else {
							logger.Error().Msgf("Error reading from unix socket: %v", err)
						}
						break
					} else {
						// Send data to the gRPC stream
						if err := stream.Send(&k8shelldpb.UnixSocketResponse{
							Response: &k8shelldpb.UnixSocketResponse_Data{Data: buf[:n]},
						}); err != nil {
							logger.Error().Msgf("Failed to send data to the client: %v", err)
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
				logger.Info().Msg("Client closed the stream")
			} else {
				logger.Error().Msgf("Failed to receive: %v", err)
			}
			break
		}

		unixsocket.mu.Lock()
		conn := unixsocket.conn
		unixsocket.mu.Unlock()

		if conn != nil {
			data := req.GetData()
			if _, err := conn.Write(data); err != nil {
				logger.Error().Msgf("Failed to write data to the unix socket: %v", err)
				break
			}
			unixsocket.BytesIn += uint64(len(data))
		}
	}

	logger.Info().Msg("Communication ended")
	unixsocket.listener.Close()
	return nil
}

func (s *RemoteOSServiceServer) sendUnixSocketTerminate(stream k8shelldpb.RemoteOSService_UnixSocketServer) error {
	return stream.Send(&k8shelldpb.UnixSocketResponse{Response: &k8shelldpb.UnixSocketResponse_Terminate{Terminate: true}})
}
