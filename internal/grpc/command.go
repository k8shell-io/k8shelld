// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package grpc

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CommandServiceServer implements the gRPC server for command execution.
//
// It manages a single active logical connection (bidi stream) from a client.
// Other parts of the server can send commands to that client via SendCommand,
// and await the reply. Replies from the client are correlated using command_id.
type CommandServiceServer struct {
	k8shelldv1.UnimplementedCommandServiceServer

	logger *zerolog.Logger

	mu         sync.Mutex
	clients    map[uint64]chan *k8shelldv1.CommandMessage
	nextClient uint64
	pending    map[string]chan string
}

// NewCommandServiceServer creates a new CommandServiceServer instance.
func NewCommandServiceServer() *CommandServiceServer {
	return &CommandServiceServer{
		logger:  logger.NewLogger("grpc-commands"),
		clients: make(map[uint64]chan *k8shelldv1.CommandMessage),
		pending: make(map[string]chan string),
	}
}

// CommandListener implements the bidi streaming RPC defined in the proto.
// Only a single active client is supported at a time; a new connection replaces any previous one.
func (s *CommandServiceServer) CommandListener(stream k8shelldv1.CommandService_CommandListenerServer) error {
	ctx := stream.Context()

	// Create a send channel dedicated to this stream instance and register client.
	ch := make(chan *k8shelldv1.CommandMessage, 16)

	s.mu.Lock()
	clientID := s.nextClient
	s.nextClient++
	s.clients[clientID] = ch
	s.mu.Unlock()

	s.logger.Info().Uint64("client_id", clientID).Msg("command listener connected")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range ch {
			if err := stream.Send(msg); err != nil {
				if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
					s.logger.Debug().Uint64("client_id", clientID).Msg("command stream send canceled by client")
				} else {
					s.logger.Error().Uint64("client_id", clientID).Err(err).Msg("failed to send command to client")
				}
				return
			}
		}
	}()

	for {
		in, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
				s.logger.Debug().Uint64("client_id", clientID).Msg("command stream canceled by client")
				break
			}
			s.logger.Error().Uint64("client_id", clientID).Err(err).Msg("error receiving command reply from client")
			break
		}

		cmdID := in.GetCommandId()
		switch payload := in.Payload.(type) {
		case *k8shelldv1.CommandMessage_Reply:
			reply := payload.Reply

			s.mu.Lock()
			chReply, ok := s.pending[cmdID]
			if ok {
				delete(s.pending, cmdID)
			}
			s.mu.Unlock()

			if ok {
				select {
				case chReply <- reply:
					close(chReply)
				default:
					// receiver gone; drop reply
				}
			} else {
				s.logger.Warn().Str("command_id", cmdID).Uint64("client_id", clientID).
					Msg("received reply for unknown command_id")
			}
		default:
			s.logger.Warn().Uint64("client_id", clientID).
				Msg("received unexpected command payload type from client; ignoring")
		}
	}

	s.mu.Lock()
	if chCurrent, ok := s.clients[clientID]; ok && chCurrent == ch {
		close(ch)
		delete(s.clients, clientID)
	}
	s.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
	}

	s.logger.Info().Uint64("client_id", clientID).Msg("command listener disconnected")
	return nil
}

// SendCommand sends a command to exactly one connected client and waits for reply.
func (s *CommandServiceServer) SendCommand(ctx context.Context, command string) (string, error) {
	if command == "" {
		return "", errors.New("empty command")
	}

	s.mu.Lock()
	// pick any one client (first in map) to send the command to
	var sendCh chan *k8shelldv1.CommandMessage
	for _, ch := range s.clients {
		sendCh = ch
		break
	}
	if sendCh == nil {
		s.mu.Unlock()
		return "", status.Errorf(codes.Unavailable, "no command client connected")
	}

	cmdID := strconv.FormatInt(time.Now().UnixNano(), 10)
	replyCh := make(chan string, 1)
	s.pending[cmdID] = replyCh
	s.mu.Unlock()

	msg := &k8shelldv1.CommandMessage{
		CommandId: cmdID,
		Payload: &k8shelldv1.CommandMessage_Command{
			Command: command,
		},
	}

	// Send the command to the selected client.
	select {
	case sendCh <- msg:
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, cmdID)
		s.mu.Unlock()
		return "", ctx.Err()
	}

	// Wait for the reply or cancellation.
	select {
	case reply := <-replyCh:
		if strings.HasPrefix(reply, "error:") {
			return "", errors.New(strings.TrimSpace(strings.TrimPrefix(reply, "error:")))
		}
		return reply, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, cmdID)
		s.mu.Unlock()
		return "", ctx.Err()
	}
}
