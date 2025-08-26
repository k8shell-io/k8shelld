package grpc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
	"github.com/rs/zerolog"

	"github.com/creack/pty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SessionData stores the data of a shell session.
type SessionData struct {
	Id       string
	user     system.User
	CmdShell string
	Cmd      *exec.Cmd
	Ptmx     *os.File
	Pid      int
	Created  time.Time
	Deleted  time.Time
	BytesIn  uint64
	BytesOut uint64
}

// ShellServiceServer is the service that handles the shell GRPC service server
type ShellServiceServer struct {
	grpcApi *GRPCService
	logger  *zerolog.Logger
	k8shelldpb.UnimplementedShellServiceServer
}

// streamWriter is a writer that sends the data to the client stream
type streamWriter struct {
	stream k8shelldpb.ShellService_ShellServer
}

// Write writes the data to the client stream
func (sw *streamWriter) Write(data []byte) (int, error) {
	err := sw.stream.Send(&k8shelldpb.ShellResponse{Response: &k8shelldpb.ShellResponse_Data{Data: data}})
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// NewShellServiceServer creates a new ShellServiceServer
func NewShellServiceServer(grpcapi *GRPCService) *ShellServiceServer {
	return &ShellServiceServer{
		grpcApi: grpcapi,
		logger:  log.NewLogger("grpc-shell"),
	}
}

// getUserLoginShell verify if the shell is valid
func isValidShell(shell string) bool {
	file, err := os.Open("/etc/shells")
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			if scanner.Text() == shell {
				return true
			}
		}
	}
	return false
}

// Get the port-forward ID from the gRPC metadata "portforward-id"
func (s *ShellServiceServer) GetSessionID(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "missing metadata")
	}

	data := md.Get("session-id")
	if len(data) == 0 {
		return "", status.Errorf(codes.InvalidArgument, "missing session-id")
	}

	return data[0], nil
}

// Get the port-forward data from the store. It uses the port-forward ID retrieved from the metadata
func (s *ShellServiceServer) GetSessionData(ctx context.Context) (*SessionData, error) {
	sid, err := s.GetSessionID(ctx)
	if err != nil {
		return nil, err
	}
	value, ok := s.grpcApi.SessionStore.Load(sid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "session-id %s not found", sid)
	}
	return value.(*SessionData), nil
}

// Shell is a gRPC method that starts a shell session. It is a bidirectional streaming RPC
// that sends the shell output to the client and receives the client input to send to the shell.
func (s *ShellServiceServer) Shell(stream k8shelldpb.ShellService_ShellServer) error {
	sessionId, err := s.GetSessionID(stream.Context())
	if err != nil {
		return fmt.Errorf("failed to get session ID: %v", err)
	}

	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive shell request: %v", err)
	}

	shellReq, ok := req.Request.(*k8shelldpb.ShellRequest_StartRequest)
	if !ok {
		return status.Errorf(codes.InvalidArgument, "invalid shell request: %v", req)
	}

	// Get the login shell for the user
	shell, err := system.GetUserLoginShell(s.grpcApi.user.Username)
	if err != nil {
		shell = shellReq.StartRequest.CmdShell
	}

	session := &SessionData{
		Id:       sessionId,
		user:     s.grpcApi.user,
		CmdShell: shell,
		Pid:      -1,
		Created:  time.Now(),
		Deleted:  time.Time{},
		BytesIn:  0,
		BytesOut: 0,
	}

	// Start the shell process
	session.Cmd = exec.Command(shell)
	session.Cmd.Args[0] = "-" + session.Cmd.Args[0] // make the shell a login shell

	session.Cmd.Env = system.CreateEnvVars(shellReq.StartRequest.SetEnvVars, session.user.HomeDir)
	session.Cmd.Dir = session.user.HomeDir

	s.logger.Debug().Msgf("env: %v", session.Cmd.Env)

	// Set process attributes for the shell
	session.Cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create a new process group
		Credential: &syscall.Credential{
			Uid:    uint32(session.user.Uid),
			Gid:    uint32(session.user.Gid),
			Groups: system.GetSupplementalGroups(session.user.Username),
		},
	}

	s.grpcApi.SessionStore.Store(session.Id, session)

	defer func() {
		s.cleanUpSession(session)
		session.Deleted = time.Now()
		s.logger.Info().Msgf("Shell session %s ended", sessionId)
	}()

	s.logger.Info().Msgf("Starting shell session %s, pty=%v", sessionId, shellReq.StartRequest.UsePty)

	// Start the shell
	if shellReq.StartRequest.UsePty {
		err = s.handlePtySession(s.logger, session, stream, shellReq.StartRequest.Width, shellReq.StartRequest.Height)
		if err != nil {
			return fmt.Errorf("error handling PTY session: %v", err)
		}
	} else {
		err = s.handleNonPtySession(s.logger, session, stream)
		if err != nil {
			return fmt.Errorf("error handling non-PTY session: %v", err)
		}
	}

	return nil

}

// cleanUpSession cleans up the session by killing the shell process and closing the PTY
func (s *ShellServiceServer) cleanUpSession(session *SessionData) {
	if session.Cmd != nil {
		if session.Cmd.Process.Pid != 0 {
			_ = syscall.Kill(-session.Cmd.Process.Pid, syscall.SIGKILL)
			_ = session.Cmd.Process.Kill()
			session.Cmd = nil
		}
	}
	if session.Ptmx != nil {
		session.Ptmx.Close()
	}
}

// handlePtySession handles a shell session with PTY. It creates the PTY session, sets the width and height of the terminal,
// reads data from the PTY and sends the data back to the client and vice versa.
func (s *ShellServiceServer) handlePtySession(logger *zerolog.Logger, session *SessionData,
	stream k8shelldpb.ShellService_ShellServer, width uint32, height uint32) error {

	var err error
	session.Ptmx, err = pty.Start(session.Cmd)
	if err != nil {
		return fmt.Errorf("error starting the shell with PTY: %v", err)
	}

	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(session.Cmd.Process.Pid)
	session.Pid = session.Cmd.Process.Pid

	if width > 0 && height > 0 {
		pty.Setsize(session.Ptmx, &pty.Winsize{
			Rows: uint16(height),
			Cols: uint16(width),
		})
	}

	ctx := stream.Context()
	reqCh := make(chan *k8shelldpb.ShellRequest, 8)
	recvErrCh := make(chan error, 1)
	ptyDone := make(chan struct{})

	// PTY -> client
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := session.Ptmx.Read(buf)
			if err != nil {
				close(ptyDone)
				return
			}
			if n > 0 {
				if sendErr := stream.Send(&k8shelldpb.ShellResponse{
					Response: &k8shelldpb.ShellResponse_Data{Data: append([]byte(nil), buf[:n]...)},
				}); sendErr != nil {
					recvErrCh <- sendErr // surface error; main will return
					return
				}
			}
		}
	}()

	// client -> PTY
	go func() {
		defer close(reqCh) // signal consumer to stop when Recv ends
		for {
			req, err := stream.Recv()
			if err != nil {
				recvErrCh <- err // io.EOF or canceled or real error
				return
			}
			reqCh <- req
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// client canceled; returning will close server->client side
			return nil

		case <-ptyDone:
			// PTY ended; finish RPC so client Recv() sees EOF
			return nil

		case err := <-recvErrCh:
			if err == io.EOF {
				logger.Info().Msg("client closed stream")
				return nil
			}
			// propagate other errors (or log and return nil if you prefer)
			return fmt.Errorf("stream error: %w", err)

		case req, ok := <-reqCh:
			if !ok {
				// Recv goroutine ended; nothing more to read from client
				return nil
			}
			if data := req.GetData(); data != nil {
				if _, werr := session.Ptmx.Write(data); werr != nil {
					return fmt.Errorf("pty write: %w", werr)
				}
			}
		}
	}
}

// handleNonPtySession handles a shell session without PTY. It creates pipes for the stdin, stdout and stderr of the
// shell process, reads data from the pipes and sends the data back to the client and vice versa.
func (s *ShellServiceServer) handleNonPtySession(logger *zerolog.Logger, session *SessionData,
	stream k8shelldpb.ShellService_ShellServer) error {

	var stdoutPipe io.ReadCloser
	var stderrPipe io.ReadCloser
	var stdinPipe io.WriteCloser
	var err error

	defer func() {
		if stdoutPipe != nil {
			stdoutPipe.Close()
		}
		if stderrPipe != nil {
			stderrPipe.Close()
		}
		if stdinPipe != nil {
			stdinPipe.Close()
		}
		logger.Debug().Msgf("Pipes closed in session %s", session.Id)
	}()

	stdoutPipe, err = session.Cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to set up stdout pipe: %v", err)
	}
	stderrPipe, err = session.Cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to set up stderr pipe: %v", err)
	}
	stdinPipe, err = session.Cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to set up stdin pipe: %v", err)
	}

	if err := session.Cmd.Start(); err != nil {
		return fmt.Errorf("error starting shell session %s (no-pty): %v", session.Id, err)
	}

	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(session.Cmd.Process.Pid)
	session.Pid = session.Cmd.Process.Pid

	// goroutine to read from stdout and stderr pipes and send the data to the client
	// When the pipes are closed, it sends a terminate message to the client
	// which will close the client stream
	go func() {
		_, err = io.Copy(&streamWriter{stream: stream}, stdoutPipe)
		if err != nil {
			logger.Error().Msgf("Error writing to stdout: %v", err)
		}
		logger.Debug().Msgf("Closing stdout pipe, session %s", session.Id)
		//s.sendShellTerminate(stream)
	}()

	go func() {
		_, err = io.Copy(&streamWriter{stream: stream}, stderrPipe)
		if err != nil {
			logger.Error().Msgf("Error writing to stderr: %v", err)
		}
		logger.Debug().Msgf("Closing stderr pipe, session %s", session.Id)
		//s.sendShellTerminate(stream)
	}()

	// Read from the client stream and write to the stdin pipe
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			logger.Info().Msgf("Client closed the stream")
			break
		}
		if err != nil {
			return fmt.Errorf("failed to receive: %v", err)
		}

		data := req.GetData()
		if data != nil {
			_, writeErr := stdinPipe.Write(data)
			if writeErr != nil {
				logger.Error().Msgf("Error writing to stdin: %v", writeErr)
				break
			}
		} else {
			logger.Error().Msg("received empty data")
			return fmt.Errorf("received empty data")
		}
	}

	return nil
}

// ResizeTerminal is a gRPC method that resizes the terminal of a shell session.
func (s *ShellServiceServer) ResizeTerminal(ctx context.Context,
	req *k8shelldpb.ResizeTerminalRequest) (*k8shelldpb.ResizeTerminalResponse, error) {
	session, err := s.GetSessionData(ctx)
	if err != nil {
		return nil, err
	}

	s.logger.Debug().Msgf("Resizing shell session %s, cols: %d, rows: %d", session.Id, req.Width, req.Height)

	pty.Setsize(session.Ptmx, &pty.Winsize{
		Rows: uint16(req.Height),
		Cols: uint16(req.Width),
	})
	return &k8shelldpb.ResizeTerminalResponse{}, nil
}

// sendShellTerminate sends a terminate message to the client to close the shell session
func (s *ShellServiceServer) sendShellTerminate(stream k8shelldpb.ShellService_ShellServer) error {
	return stream.Send(&k8shelldpb.ShellResponse{Response: &k8shelldpb.ShellResponse_Terminate{Terminate: true}})
}
