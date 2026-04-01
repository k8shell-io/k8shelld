package grpc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/internal/utils"
	"github.com/rs/zerolog"

	"github.com/creack/pty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SessionData stores the data of a shell session.
type SessionData struct {
	Id           string
	user         models.ShellUser
	CmdShell     string
	Cmd          *exec.Cmd
	Ptmx         *os.File
	Pid          int
	Created      time.Time
	Deleted      time.Time
	BytesIn      uint64
	BytesOut     uint64
	DockerExecID string    // exec ID when running inside a Docker container; empty for local shell
	dockerClient io.Closer // *dockerclient.Client; non-nil when DockerExecID is set
}

// ShellServiceServer is the service that handles the shell GRPC service server
type ShellServiceServer struct {
	grpcApi *GRPCService
	logger  *zerolog.Logger
	k8shelldv1.UnimplementedShellServiceServer
}

// streamWriter is a writer that sends the data to the client stream
type streamWriter struct {
	stream k8shelldv1.ShellService_ShellServer
}

// Write writes the data to the client stream
func (sw *streamWriter) Write(data []byte) (int, error) {
	err := sw.stream.Send(&k8shelldv1.ShellResponse{Response: &k8shelldv1.ShellResponse_Data{Data: data}})
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// NewShellServiceServer creates a new ShellServiceServer
func NewShellServiceServer(grpcapi *GRPCService) *ShellServiceServer {
	return &ShellServiceServer{
		grpcApi: grpcapi,
		logger:  logger.NewLogger("grpc-shell"),
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

// Get the session data from the store. It uses the session ID retrieved from the metadata
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
func (s *ShellServiceServer) Shell(stream k8shelldv1.ShellService_ShellServer) error {
	sessionId, err := s.GetSessionID(stream.Context())
	if err != nil {
		return fmt.Errorf("failed to get session ID: %v", err)
	}

	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive shell request: %v", err)
	}

	shellReq, ok := req.Request.(*k8shelldv1.ShellRequest_StartRequest)
	if !ok {
		return status.Errorf(codes.InvalidArgument, "invalid shell request: %v", req)
	}

	if shellReq.StartRequest.ContainerRef != "" {
		return s.runDockerShell(stream, sessionId, shellReq.StartRequest)
	}

	shellUser, resolveErr := s.grpcApi.resolveShellUser(shellReq.StartRequest.AsUser, s.grpcApi.user)
	if resolveErr != nil {
		s.logger.Error().Msgf("Shell session %s: error resolving user: %v", sessionId, resolveErr)
		return resolveErr
	}

	shell, err := system.GetUserLoginShell(shellUser.Username)
	if err != nil {
		shell = shellReq.StartRequest.CmdShell
	}

	if _, statErr := os.Stat(shell); statErr != nil {
		s.logger.Warn().Msgf("Shell %s not found, falling back to /bin/sh", shell)
		shell = "/bin/sh"
	}

	session := &SessionData{
		Id:       sessionId,
		user:     shellUser,
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

	session.Cmd.Env = system.CreateEnvVars(shellReq.StartRequest.SetEnvVars,
		session.user.HomeDir)
	session.Cmd.Dir = session.user.HomeDir

	s.logger.Debug().Msgf("env: %v", session.Cmd.Env)

	// Set process attributes for the shell
	session.Cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create a new process group
		Credential: &syscall.Credential{
			Uid:    session.user.UID,
			Gid:    session.user.GID,
			Groups: system.GetSupplementalGroups(session.user.Username),
		},
	}

	s.grpcApi.SessionStore.Store(session.Id, session)

	defer func() {
		s.cleanUpSession(session)
		session.Deleted = time.Now()
		s.logger.Info().Msgf("Shell session %s ended", sessionId)
	}()

	s.logger.Info().Msgf("Starting shell session %s, pty=%v",
		sessionId, shellReq.StartRequest.UsePty)

	// Start the shell
	if shellReq.StartRequest.UsePty {
		err = s.handlePtySession(s.logger, session, stream, shellReq.StartRequest.Width,
			shellReq.StartRequest.Height)
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
		if session.Cmd.Process != nil && session.Cmd.Process.Pid != 0 {
			_ = syscall.Kill(-session.Cmd.Process.Pid, syscall.SIGKILL)
			_ = session.Cmd.Process.Kill()
		}
		session.Cmd = nil
	}
	if session.Ptmx != nil {
		session.Ptmx.Close()
	}
	if session.dockerClient != nil {
		_ = session.dockerClient.Close()
		session.dockerClient = nil
	}
}

// handlePtySession handles a shell session with PTY. It creates the PTY session, sets the width and height
// of the terminal, reads data from the PTY and sends the data back to the client and vice versa.
func (s *ShellServiceServer) handlePtySession(logger *zerolog.Logger, session *SessionData,
	stream k8shelldv1.ShellService_ShellServer, width uint32, height uint32) error {

	ptmx, tty, err := pty.Open()
	if err != nil {
		return fmt.Errorf("error opening PTY: %v", err)
	}
	ttyName := tty.Name()

	session.Cmd.Stdin = tty
	session.Cmd.Stdout = tty
	session.Cmd.Stderr = tty
	session.Cmd.SysProcAttr.Setctty = true
	session.Cmd.SysProcAttr.Ctty = 1

	if err = session.Cmd.Start(); err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		return fmt.Errorf("error starting shell with PTY: %v", err)
	}
	_ = tty.Close()
	session.Ptmx = ptmx

	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(session.Cmd.Process.Pid)
	session.Pid = session.Cmd.Process.Pid

	if width > 0 && height > 0 {
		if err = pty.Setsize(session.Ptmx, &pty.Winsize{
			Rows: utils.ClampUint32ToUint16(height),
			Cols: utils.ClampUint32ToUint16(width),
		}); err != nil {
			s.logger.Error().Msgf("Failed to set PTY size: %v", err)
		}
	}

	_ = stream.Send(&k8shelldv1.ShellResponse{
		Response: &k8shelldv1.ShellResponse_StartResponse{
			StartResponse: &k8shelldv1.ShellStartResponse{Pty: ttyName},
		},
	})

	ctx := stream.Context()
	reqCh := make(chan *k8shelldv1.ShellRequest, 8)
	recvErrCh := make(chan error, 1)
	ptyDone := make(chan struct{})

	if s.grpcApi.Config.Splash != "" {
		_ = stream.Send(&k8shelldv1.ShellResponse{
			Response: &k8shelldv1.ShellResponse_Data{
				Data: []byte("\n\r" + s.grpcApi.Config.ExpandSplash(s.grpcApi.user, session.user.Username) + "\n\r"),
			},
		})
	}

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
				if sendErr := stream.Send(&k8shelldv1.ShellResponse{
					Response: &k8shelldv1.ShellResponse_Data{Data: append([]byte(nil), buf[:n]...)},
				}); sendErr != nil {
					recvErrCh <- sendErr
					return
				}
				session.BytesOut += utils.SafeIntToUint64(n)
			}
		}
	}()

	// client -> PTY
	go func() {
		defer close(reqCh)
		for {
			req, err := stream.Recv()
			if err != nil {
				recvErrCh <- err
				return
			}
			reqCh <- req
		}
	}()

	for {
		select {
		case <-ctx.Done():
			s.logger.Debug().Msg("context done, closing shell session")
			return nil

		case <-ptyDone:
			s.logger.Debug().Msg("PTY closed, closing shell session")
			return nil

		case err := <-recvErrCh:
			if err == io.EOF {
				logger.Debug().Msg("client closed stream")
				return nil
			}
			return fmt.Errorf("stream error: %w", err)

		case req, ok := <-reqCh:
			if !ok {
				s.logger.Debug().Msg("Recv goroutine ended; nothing more to read from client")
				return nil
			}
			if data := req.GetData(); data != nil {
				if _, werr := session.Ptmx.Write(data); werr != nil {
					return fmt.Errorf("pty write: %w", werr)
				}
				session.BytesIn += uint64(len(data))
			}
		}
	}
}

// handleNonPtySession handles a shell session without PTY. It creates pipes for the stdin, stdout and stderr of the
// shell process, reads data from the pipes and sends the data back to the client and vice versa.
func (s *ShellServiceServer) handleNonPtySession(
	logger *zerolog.Logger,
	session *SessionData,
	stream k8shelldv1.ShellService_ShellServer,
) error {
	var (
		stdout io.ReadCloser
		stderr io.ReadCloser
		stdin  io.WriteCloser
		err    error
	)

	if stdout, err = session.Cmd.StdoutPipe(); err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if stderr, err = session.Cmd.StderrPipe(); err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if stdin, err = session.Cmd.StdinPipe(); err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	if err := session.Cmd.Start(); err != nil {
		return fmt.Errorf("start non-pty session %s: %w", session.Id, err)
	}
	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(session.Cmd.Process.Pid)
	session.Pid = session.Cmd.Process.Pid

	_ = stream.Send(&k8shelldv1.ShellResponse{
		Response: &k8shelldv1.ShellResponse_StartResponse{
			StartResponse: &k8shelldv1.ShellStartResponse{},
		},
	})

	defer func() {
		if stdout != nil {
			_ = stdout.Close()
		}
		if stderr != nil {
			_ = stderr.Close()
		}
		if stdin != nil {
			_ = stdin.Close()
		}
		logger.Debug().Msgf("pipes closed in session %s", session.Id)
	}()

	ctx := stream.Context()

	// Channels to coordinate
	reqCh := make(chan *k8shelldv1.ShellRequest, 8)
	recvErrCh := make(chan error, 1)
	clientClosed := make(chan struct{}, 1)
	outDone := make(chan struct{})

	// Server -> Client: stream stdout/stderr
	outWg := &sync.WaitGroup{}
	outWg.Add(2)

	go func() {
		defer outWg.Done()
		if _, cErr := io.Copy(&streamWriter{stream: stream}, stdout); cErr != nil {
			logger.Error().Msgf("stdout copy error (session %s): %v", session.Id, cErr)
			recvErrCh <- cErr
			return
		}
		logger.Debug().Msgf("stdout closed (session %s)", session.Id)
	}()

	go func() {
		defer outWg.Done()
		if _, cErr := io.Copy(&streamWriter{stream: stream}, stderr); cErr != nil {
			logger.Error().Msgf("stderr copy error (session %s): %v", session.Id, cErr)
			recvErrCh <- cErr
			return
		}
		logger.Debug().Msgf("stderr closed (session %s)", session.Id)
	}()

	// Notifier for when both outputs are done
	go func() {
		outWg.Wait()
		close(outDone)
	}()

	// Client -> Server: recv and write to stdin
	go func() {
		defer close(reqCh)
		for {
			req, rErr := stream.Recv()
			if rErr != nil {
				if rErr == io.EOF {
					logger.Debug().Msgf("client closed send; closing stdin (session %s)", session.Id)
					_ = stdin.Close()
					clientClosed <- struct{}{}
					return
				}
				recvErrCh <- fmt.Errorf("recv: %w", rErr)
				return
			}
			reqCh <- req
		}
	}()

	// coordination loop
	for {
		select {
		case <-ctx.Done():
			logger.Debug().Msg("context done; ending non-pty session")
			return nil

		case <-outDone:
			// Both stdout and stderr drained; process should be done (or will be).
			return nil

		case <-clientClosed:
			// Client half-closed; keep running until outputs are drained (outDone).
			continue

		case err := <-recvErrCh:
			// Any fatal error (send/recv/copy) ends the session.
			return err

		case req, ok := <-reqCh:
			if !ok {
				// recv goroutine ended without error (EOF path already handled)
				// wait for outDone to finish draining outputs.
				continue
			}
			if data := req.GetData(); data != nil {
				if _, wErr := stdin.Write(data); wErr != nil {
					return fmt.Errorf("stdin write: %w", wErr)
				}
				session.BytesIn += uint64(len(data))
			} else {
				return fmt.Errorf("received empty data")
			}
		}
	}
}

// ResizeTerminal is a gRPC method that resizes the terminal of a shell session.
func (s *ShellServiceServer) ResizeTerminal(ctx context.Context,
	req *k8shelldv1.ResizeTerminalRequest) (*k8shelldv1.ResizeTerminalResponse, error) {
	session, err := s.GetSessionData(ctx)
	if err != nil {
		return nil, err
	}

	s.logger.Debug().Msgf("Resizing shell session %s, cols: %d, rows: %d", session.Id, req.Width, req.Height)

	if session.DockerExecID != "" {
		if resizeErr := s.resizeDockerTerminal(ctx, session, req.Width, req.Height); resizeErr != nil {
			s.logger.Error().Msgf("Failed to resize docker terminal: %v", resizeErr)
		}
		return &k8shelldv1.ResizeTerminalResponse{}, nil
	}

	err = pty.Setsize(session.Ptmx, &pty.Winsize{
		Rows: utils.ClampUint32ToUint16(req.Height),
		Cols: utils.ClampUint32ToUint16(req.Width),
	})
	if err != nil {
		s.logger.Error().Msgf("Failed to resize terminal: %v", err)
	}
	return &k8shelldv1.ResizeTerminalResponse{}, nil
}
