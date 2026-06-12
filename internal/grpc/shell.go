package grpc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/display"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/internal/utils"
	"github.com/rs/zerolog"

	"github.com/creack/pty"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SessionData stores the data of a shell session.
type SessionData struct {
	Id       string
	user     models.ShellUser
	CmdShell string
	Cmd      *exec.Cmd
	Ptmx     *os.File
	Pid      int
	Created  time.Time
	Deleted  time.Time
	BytesIn  uint64
	BytesOut uint64

	// PTY session state (nil/zero for non-PTY sessions)
	DetachedAt      time.Time      // set when a client detaches from a live session
	DetachTTL       *time.Duration // per-session TTL override; nil = use server default; 0 = never expire
	ring            *RingBuffer    // scrollback buffer
	ptyDone         chan struct{}  // closed by startPtyReadLoop when the shell exits
	attachedSender  streamSender   // current live stream writer; nil when no client attached
	detachRequested chan struct{}  // per-attachment channel; closed to trigger a detach
	mu              sync.Mutex     // protects PTY state fields, BytesIn, BytesOut
}

// ShellHandler is the service that handles the shell GRPC service server
type ShellHandler struct {
	grpcApi *GRPCService
	logger  *zerolog.Logger
}

// streamWriter is a writer that sends the data to the client stream
type streamWriter struct {
	stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse]
}

// Write writes the data to the client stream
func (sw *streamWriter) Write(data []byte) (int, error) {
	err := sw.stream.Send(&k8shelldv1.ShellResponse{Response: &k8shelldv1.ShellResponse_Data{Data: data}})
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// newShellHandler creates a new ShellHandler
func newShellHandler(grpcapi *GRPCService) *ShellHandler {
	return &ShellHandler{
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
		if scanner.Err() != nil {
			return false
		}
	}
	return false
}

// Get the port-forward ID from the gRPC metadata "portforward-id"
func (s *ShellHandler) GetSessionID(ctx context.Context) (string, error) {
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
func (s *ShellHandler) GetSessionData(ctx context.Context) (*SessionData, error) {
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

// cmdStartWithTimeout calls cmd.Start() from a goroutine and waits up to
// timeout for it to return.  If it does not return in time an error is returned
// and k8shelld continues to run normally.  The goroutine remains blocked until
// the OS unblocks it (e.g. when NFS connectivity is restored); any process that
// starts successfully after the deadline is reaped by the process watcher.
//
// The primary use-case is protecting cmd.Start() against NFS hangs: when
// cmd.Dir points to an NFS-mounted home directory, the kernel chdir() inside
// Go's fork-exec may block in D-state indefinitely, which would park the
// calling goroutine forever and leave k8shelld unable to serve new requests.
func cmdStartWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	ch := make(chan error, 1)
	go func() { ch <- cmd.Start() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("process start timed out after %s (home directory may be unreachable)", timeout)
	}
}

// Shell is a gRPC method that starts a shell session. It is a bidirectional streaming RPC
// that sends the shell output to the client and receives the client input to send to the shell.
func (s *ShellHandler) Shell(stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse]) error {
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

	lockId := shellReq.StartRequest.LockId
	if lockId != "" {
		if !s.grpcApi.allowSessionDetach {
			return status.Errorf(codes.PermissionDenied, "session attachment is not enabled on this server")
		}
		if !shellReq.StartRequest.UsePty {
			return status.Errorf(codes.InvalidArgument, "lock_id requires use_pty=true")
		}
		v, loaded := s.grpcApi.SessionLockStore.LoadAndDelete(lockId)
		if !loaded {
			return status.Errorf(codes.NotFound, "lock %s not found or expired", lockId)
		}
		lock := v.(*sessionLock)
		if time.Now().After(lock.expiresAt) {
			return status.Errorf(codes.DeadlineExceeded, "lock %s has expired", lockId)
		}
		sv, exists := s.grpcApi.SessionStore.Load(lock.sessionId)
		if !exists {
			return status.Errorf(codes.NotFound, "session %s no longer exists", lock.sessionId)
		}
		return s.handleGRPCAttachExisting(stream, sv.(*SessionData), shellReq.StartRequest.DetachOnClose)
	}

	if _, exists := s.grpcApi.SessionStore.Load(sessionId); exists {
		return status.Errorf(codes.AlreadyExists, "session %s already exists", sessionId)
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
	}

	// Start the shell process
	session.Cmd = exec.Command(shell)
	session.Cmd.Args[0] = "-" + session.Cmd.Args[0] // make the shell a login shell

	session.Cmd.Env = system.CreateEnvVars(shellReq.StartRequest.SetEnvVars,
		session.user.HomeDir)
	session.Cmd.Env = append(session.Cmd.Env, "K8SHELL_SESSION_ID="+sessionId)
	// Cmd.Dir is set so the shell opens in the user's home directory.
	// cmd.Start() is called via cmdStartWithTimeout to prevent k8shelld from
	// blocking when the home directory is on an unresponsive NFS mount.
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
		session.mu.Lock()
		isDetached := !session.DetachedAt.IsZero()
		session.mu.Unlock()
		if !isDetached {
			s.cleanUpSession(session)
			session.Deleted = time.Now()
		}
		s.logger.Info().Msgf("Shell session %s ended", sessionId)
	}()

	detachOnClose := shellReq.StartRequest.DetachOnClose
	if detachOnClose && !s.grpcApi.allowSessionDetach {
		return status.Errorf(codes.PermissionDenied, "session attachment is not enabled on this server")
	}

	s.logger.Info().Msgf("Starting shell session %s, pty=%v detachOnClose=%v",
		sessionId, shellReq.StartRequest.UsePty, detachOnClose)

	if shellReq.StartRequest.UsePty {
		err = s.handlePtySession(s.logger, session, stream, shellReq.StartRequest.Width,
			shellReq.StartRequest.Height, detachOnClose, shellReq.StartRequest.GetShowInitScriptStatus())
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

// cleanup kills the shell process and closes the PTY.
// Safe to call multiple times and from concurrent goroutines.
func (session *SessionData) cleanup() {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.Cmd != nil {
		if session.Cmd.Process != nil && session.Cmd.Process.Pid != 0 {
			_ = syscall.Kill(-session.Cmd.Process.Pid, syscall.SIGKILL)
			_ = session.Cmd.Process.Kill()
		}
		session.Cmd = nil
	}
	if session.Ptmx != nil {
		_ = session.Ptmx.Close()
		session.Ptmx = nil
	}
}

// cleanUpSession cleans up the session by killing the shell process and closing the PTY
func (s *ShellHandler) cleanUpSession(session *SessionData) {
	session.cleanup()
}

// handlePtySession starts the shell with a PTY, then hands off to the
// session-owned PTY read loop and the attached-client loop.
func (s *ShellHandler) handlePtySession(logger *zerolog.Logger, session *SessionData,
	stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse],
	width uint32, height uint32, autoDetach bool, showInitScriptStatus bool) error {

	session.ring = newRingBuffer(detachableRingBufSize)
	session.ptyDone = make(chan struct{})

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

	unlockCreation := s.grpcApi.procWatcher.LockForCreation()
	if err = cmdStartWithTimeout(session.Cmd, 5*time.Second); err != nil {
		unlockCreation()
		_ = ptmx.Close()
		_ = tty.Close()
		return fmt.Errorf("error starting shell with PTY: %v", err)
	}
	_ = tty.Close()
	session.Ptmx = ptmx

	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(session.Cmd.Process.Pid)
	session.Pid = session.Cmd.Process.Pid
	unlockCreation()

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

	if s.grpcApi.blueprint != nil && s.grpcApi.blueprint.Splash != "" {
		splashData := []byte("\n\r" + config.ExpandSplash(s.grpcApi.blueprint.Splash, session.user.Username) + "\n\r")
		_ = stream.Send(&k8shelldv1.ShellResponse{
			Response: &k8shelldv1.ShellResponse_Data{
				Data: splashData,
			},
		})
		session.ring.Write(splashData)
	}

	s.startPtyReadLoop(session)

	var showProgress bool
	if showInitScriptStatus {
		if s.grpcApi.blueprint != nil && s.grpcApi.blueprint.ShowInitScriptStatus {
			showProgress = true
		} else {
			logger.Warn().Msg("client requested show_init_script_status but blueprint has showInitScriptStatus=false; init script progress will not be displayed")
		}
	}

	var preReadCh <-chan shellRecvMsg
	if showProgress && s.grpcApi.initTracker != nil && s.grpcApi.initTracker.HasPendingOrRunning() {
		session.ring.Mark()
		preReadCh = s.showInitProgress(stream, session)
		if scrollback := session.ring.SnapshotSinceMark(); len(scrollback) > 0 {
			_ = stream.Send(&k8shelldv1.ShellResponse{
				Response: &k8shelldv1.ShellResponse_Data{Data: stripTerminalQueryResponses(scrollback)},
			})
		}
		for _, l := range display.RenderInitProgress(s.grpcApi.initTracker.GetAll(), "", true, true) {
			session.ring.Write([]byte(l + "\r\n"))
		}
		session.ring.Write([]byte("\r\n"))
	}

	detachCh := session.doAttach(&grpcStreamSender{stream: stream})
	return s.runAttachedClientLoop(logger, session, stream, detachCh, autoDetach, preReadCh)
}

// handleNonPtySession handles a shell session without PTY. It creates pipes for the stdin, stdout and stderr of the
// shell process, reads data from the pipes and sends the data back to the client and vice versa.
func (s *ShellHandler) handleNonPtySession(
	logger *zerolog.Logger,
	session *SessionData,
	stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse],
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

	unlockCreation := s.grpcApi.procWatcher.LockForCreation()
	if err := cmdStartWithTimeout(session.Cmd, 5*time.Second); err != nil {
		unlockCreation()
		return fmt.Errorf("start non-pty session %s: %w", session.Id, err)
	}
	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(session.Cmd.Process.Pid)
	session.Pid = session.Cmd.Process.Pid
	unlockCreation()

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
func (s *ShellHandler) ResizeTerminal(ctx context.Context,
	req *k8shelldv1.ResizeTerminalRequest) (*k8shelldv1.ResizeTerminalResponse, error) {
	session, err := s.GetSessionData(ctx)
	if err != nil {
		return nil, err
	}

	s.logger.Debug().Msgf("Resizing shell session %s, cols: %d, rows: %d", session.Id, req.Width, req.Height)

	err = pty.Setsize(session.Ptmx, &pty.Winsize{
		Rows: utils.ClampUint32ToUint16(req.Height),
		Cols: utils.ClampUint32ToUint16(req.Width),
	})
	if err != nil {
		s.logger.Error().Msgf("Failed to resize terminal: %v", err)
	}
	return &k8shelldv1.ResizeTerminalResponse{}, nil
}

// showInitProgress renders init script progress directly on the gRPC stream
// until all scripts complete or the user presses Ctrl+C.  It returns a
// pre-read channel that runAttachedClientLoop must use so no stream bytes
// are lost during the progress phase.
func (s *ShellHandler) showInitProgress(
	stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse],
	session *SessionData,
) <-chan shellRecvMsg {
	recvCh := make(chan shellRecvMsg, 32)

	go func() {
		for {
			req, err := stream.Recv()
			select {
			case recvCh <- shellRecvMsg{req, err}:
			default:
				// Buffer full: write input directly to PTY so keystrokes are
				// not lost even if the progress loop is slow to drain.
				if err == nil && req.GetData() != nil {
					_, _ = session.Ptmx.Write(req.GetData())
				}
			}
			if err != nil {
				return
			}
		}
	}()

	writer := &streamWriter{stream: stream}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	tracker := s.grpcApi.initTracker
	spinTick := 0
	lastLineCount := 0

	renderFrame := func(final bool) {
		spin := ""
		if !final {
			spin = display.SpinnerFrame(spinTick)
			spinTick++
		}
		if lastLineCount > 0 {
			display.ClearLines(writer, lastLineCount)
		}
		lines := display.RenderInitProgress(tracker.GetAll(), spin, true, true)
		for _, l := range lines {
			_, _ = writer.Write([]byte(l + "\r\n"))
		}
		lastLineCount = len(lines)
	}

	renderFrame(false)

loop:
	for {
		select {
		case msg, ok := <-recvCh:
			if !ok || msg.err != nil {
				break loop
			}
			if data := msg.req.GetData(); data != nil {
				ctrlC := false
				for _, b := range data {
					if b == 0x03 { // Ctrl+C — exit display without touching the shell
						ctrlC = true
						break
					}
				}
				if ctrlC {
					break loop
				}
				// Forward any other user input to the PTY.
				_, _ = session.Ptmx.Write(data)
			}
			if tracker.AllDone() {
				break loop
			}

		case <-ticker.C:
			if tracker.AllDone() {
				break loop
			}
			renderFrame(false)
		}
	}

	// Final render showing completed states.
	renderFrame(true)
	_, _ = writer.Write([]byte("\r\n"))

	return recvCh
}

// GetCWD returns the current working directory of the shell process identified
// by the shell_id in the request. The CWD is resolved by reading the
// /proc/<pid>/cwd symlink of the shell process.
func (s *ShellHandler) GetCWD(_ context.Context, req *k8shelldv1.GetCWDRequest) (*k8shelldv1.GetCWDResponse, error) {
	if req.GetShellId() == "" {
		return nil, status.Error(codes.InvalidArgument, "shell_id is required")
	}

	value, ok := s.grpcApi.SessionStore.Load(req.GetShellId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "shell session %s not found", req.GetShellId())
	}

	session := value.(*SessionData)
	if session.Pid <= 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"shell session %s has no running process", req.GetShellId())
	}

	cmd := exec.Command("readlink", fmt.Sprintf("/proc/%d/cwd", session.Pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:         session.user.UID,
			Gid:         session.user.GID,
			NoSetGroups: true,
		},
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to read CWD for shell %s: %v",
			req.GetShellId(), err)
	}
	cwd := strings.TrimRight(string(out), "\n")

	return &k8shelldv1.GetCWDResponse{Path: cwd}, nil
}
