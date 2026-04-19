// portforward.go, copyright 2025 the k8shell.io authors

// Port-forwarding service server. It creates a new port-forwarding instance, starts the TCP connection, and
// streams data between the client and the destination.

package grpc

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/utils"
	"github.com/rs/zerolog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// PortForwardServiceServer is the service that handles the port-forwarding GRPC service server
type PortForwardServiceServer struct {
	grpcApi *GRPCService
	logger  *zerolog.Logger
}

// Port-forward data structure
type PortForwardData struct {
	Id          string
	Destination string
	Port        uint16
	Created     time.Time
	Deleted     time.Time
	BytesIn     uint64
	BytesOut    uint64
}

// NewPortForwardServiceServer creates a new PortForwardServiceServer
func NewPortForwardServiceServer(grpcapi *GRPCService) *PortForwardServiceServer {
	return &PortForwardServiceServer{
		grpcApi: grpcapi,
		logger:  logger.NewLogger("grpc-portforward"),
	}
}

// Get the port-forward ID from the gRPC metadata "portforward-id"
func (s *PortForwardServiceServer) GetPortForwardID(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "missing metadata")
	}

	data := md.Get("portforward-id")
	if len(data) == 0 {
		return "", status.Errorf(codes.InvalidArgument, "missing portforward-id")
	}

	return data[0], nil
}

// Get the port-forward data from the store. It uses the port-forward ID retrieved from the metadata
func (s *PortForwardServiceServer) GetPortForwardData(ctx context.Context) (*PortForwardData, error) {
	pfID, err := s.GetPortForwardID(ctx)
	if err != nil {
		return nil, err
	}
	value, ok := s.grpcApi.PortForwardStore.Load(pfID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "port-forward id %s not found", pfID)
	}
	return value.(*PortForwardData), nil
}

func (s *PortForwardServiceServer) createTCPConnection(destination string, port uint16) (net.Conn, error) {
	tcpConn, err := net.Dial("tcp", net.JoinHostPort(destination, fmt.Sprintf("%d", port)))
	if err != nil {
		return nil, status.Errorf(
			codes.Unavailable,
			"failed to connect to %s:%d: %v", destination, port, err,
		)
	}
	return tcpConn, nil
}

// PortForward sets up a TCP <-> gRPC bidi bridge.
// First request must be Destination. Then we stream bytes both ways until
// client closes, TCP closes, context cancels, or an error occurs.
func (s *PortForwardServiceServer) PortForward(
	stream grpc.BidiStreamingServer[k8shelldv1.PortForwardRequest, k8shelldv1.PortForwardResponse],
) error {
	ctx := stream.Context()

	pfID, err := s.GetPortForwardID(ctx)
	if err != nil {
		return fmt.Errorf("get port-forward id: %w", err)
	}

	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receive destination: %v", err)
	}
	dstReq, ok := first.Request.(*k8shelldv1.PortForwardRequest_Destination)
	if !ok || dstReq.Destination == nil {
		return status.Errorf(codes.InvalidArgument, "invalid first request (need Destination)")
	}

	pf := &PortForwardData{
		Id:          pfID,
		Destination: dstReq.Destination.Ip,
		Port:        utils.ClampUint32ToUint16(dstReq.Destination.Port),
		Created:     time.Now(),
	}
	tcpConn, err := s.createTCPConnection(pf.Destination, pf.Port)
	if err != nil {
		return status.Errorf(codes.Unavailable, "dial %s:%d: %v", pf.Destination, pf.Port, err)
	}
	defer func() {
		_ = tcpConn.Close()
		pf.Deleted = time.Now()
		s.logger.Info().Msgf("Port-forward ended: id=%s, dur=%s, in=%d, out=%d",
			pf.Id, time.Since(pf.Created), pf.BytesIn, pf.BytesOut)
	}()

	s.grpcApi.PortForwardStore.Store(pf.Id, pf)
	s.logger.Info().Msgf("Port-forward started: id=%s -> %s:%d", pf.Id, pf.Destination, pf.Port)

	// Coordination channels
	recvErrCh := make(chan error, 2) // client recv/send/tcp write errors
	tcpErrCh := make(chan error, 1)  // tcp read/send errors

	// TCP -> gRPC (server sends to client)
	go func() {
		buf := make([]byte, config.DEFAULT_MAX_PACKET_SIZE)
		for {
			n, rerr := tcpConn.Read(buf)
			if rerr != nil {
				if rerr != io.EOF {
					s.logger.Error().Msgf("tcp read: %v", rerr)
				} else {
					s.logger.Debug().Msg("tcp read: EOF")
				}
				tcpErrCh <- rerr
				return
			}
			if n == 0 {
				continue
			}

			if serr := stream.Send(&k8shelldv1.PortForwardResponse{
				Data: append([]byte(nil), buf[:n]...),
			}); serr != nil {
				recvErrCh <- fmt.Errorf("grpc send: %w", serr)
				return
			}
			pf.BytesOut += utils.SafeIntToUint64(n)
		}
	}()

	// gRPC -> TCP (client sends to server)
	go func() {
		for {
			req, rerr := stream.Recv()
			if rerr != nil {
				recvErrCh <- rerr
				return
			}
			switch r := req.Request.(type) {
			case *k8shelldv1.PortForwardRequest_Data:
				if len(r.Data) == 0 {
					continue
				}
				if _, werr := tcpConn.Write(r.Data); werr != nil {
					recvErrCh <- fmt.Errorf("tcp write: %w", werr)
					return
				}
				pf.BytesIn += uint64(len(r.Data))
			default:
				recvErrCh <- status.Errorf(codes.InvalidArgument, "unexpected request type (want Data)")
				return
			}
		}
	}()

	// Main coordination
	for {
		select {
		case <-ctx.Done():
			return nil

		case err := <-tcpErrCh:
			if err != nil && err != io.EOF {
				s.logger.Debug().Msgf("ending due to tcp error: %v", err)
			}
			return nil

		case err := <-recvErrCh:
			if err == io.EOF {
				s.logger.Info().Msg("client closed send; finishing")
				return nil
			}
			s.logger.Error().Msgf("stream error: %v", err)
			return nil
		}
	}
}
