// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package grpc

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/rs/zerolog"

	"github.com/creack/pty"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/internal/utils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// exec data structure
type ExecData struct {
	Id       string
	Command  string
	Created  time.Time
	Deleted  time.Time
	BytesIn  uint64
	BytesOut uint64
}

// ExecHandler is the service that handles the exec GRPC service server
type ExecHandler struct {
	grpcApi *GRPCService
	logger  *zerolog.Logger
}

// newExecHandler creates a new ExecHandler
func newExecHandler(grpcapi *GRPCService) *ExecHandler {
	return &ExecHandler{
		grpcApi: grpcapi,
		logger:  logger.NewLogger("grpc-exec"),
	}
}

// Get the exec ID from the gRPC metadata "exec-id"
func (s *ExecHandler) GetExecID(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "missing metadata")
	}

	data := md.Get("exec-id")
	if len(data) == 0 {
		return "", status.Errorf(codes.InvalidArgument, "missing exec-id")
	}

	return data[0], nil
}

// Exec runs a command as `<shell> -c <command>`, like sshd, optionally attached
// to a PTY. The shell is the shell_binary hint if it exists in the workspace,
// otherwise the user's passwd login shell, otherwise /bin/sh.
func (s *ExecHandler) Exec(stream grpc.BidiStreamingServer[k8shelldv1.ExecRequest, k8shelldv1.ExecResponse]) error {
	var cmd *exec.Cmd
	var exitCode int32

	execId, err := s.GetExecID(stream.Context())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to get exec ID: %v", err)
	}

	_, ok := s.grpcApi.ExecStore.Load(execId)
	if ok {
		return status.Errorf(codes.AlreadyExists, "exec-id %s already exists", execId)
	}

	// The first request is the command
	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive command: %v", err)
	}

	cmdReq, ok := req.Request.(*k8shelldv1.ExecRequest_CommandDetails)
	if !ok {
		return status.Errorf(codes.InvalidArgument, "invalid command request: %v", req)
	}
	details := cmdReq.CommandDetails

	if strings.TrimSpace(details.Command) == "" {
		return status.Errorf(codes.InvalidArgument, "empty command")
	}

	shellUser, resolveErr := s.grpcApi.resolveShellUser(details.AsUser, s.grpcApi.user)
	if resolveErr != nil {
		s.logger.Error().Msgf("Exec session %s: error resolving user: %v", execId, resolveErr)
		return resolveErr
	}

	// Commands prefer the client's shell hint over the passwd login shell.
	shell := resolveShell(details.ShellBinary, loginShell(shellUser.Username))
	if details.ShellBinary != "" && shell != details.ShellBinary {
		s.logger.Debug().Msgf("Exec session %s: shell %s not found, using %s", execId, details.ShellBinary, shell)
	}

	cmd = exec.Command(shell, "-c", details.Command)
	cmd.Env = userEnv(details.EnvVars, shellUser, shell)
	cmd.Dir = shellUser.HomeDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create a new process group
		Credential: &syscall.Credential{
			Uid:    shellUser.UID,
			Gid:    shellUser.GID,
			Groups: system.GetSupplementalGroups(shellUser.Username),
		},
	}

	// stream.Send is not safe for concurrent use; output goroutines and the
	// main goroutine all send.
	var sendMu sync.Mutex
	send := func(resp *k8shelldv1.ExecResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(resp)
	}
	stdoutResp := func(b []byte) *k8shelldv1.ExecResponse {
		return &k8shelldv1.ExecResponse{Response: &k8shelldv1.ExecResponse_Stdout{Stdout: b}}
	}
	stderrResp := func(b []byte) *k8shelldv1.ExecResponse {
		return &k8shelldv1.ExecResponse{Response: &k8shelldv1.ExecResponse_Stderr{Stderr: b}}
	}

	// With a PTY, stdin/stdout/stderr all go through the PTY master and output
	// is sent as stdout only, like a real terminal. Without one, use pipes.
	var (
		ptmx      *os.File
		tty       *os.File
		stdin     io.WriteCloser
		outputs   []execOutput
		childEnds []*os.File // closed in the parent once the child has them
	)
	if details.UsePty {
		ptmx, tty, err = pty.Open()
		if err != nil {
			return status.Errorf(codes.Internal, "failed to open PTY: %v", err)
		}
		defer ptmx.Close()
		cmd.Stdin = tty
		cmd.Stdout = tty
		cmd.Stderr = tty
		cmd.SysProcAttr.Setctty = true
		cmd.SysProcAttr.Ctty = 0
		childEnds = []*os.File{tty}
		stdin = ptmx
		outputs = []execOutput{{name: "pty", r: ptmx, resp: stdoutResp}}
	} else {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return status.Errorf(codes.Internal, "failed to create stdin pipe: %v", err)
		}
		// os.Pipe rather than StdoutPipe/StderrPipe: cmd.Wait closes the read
		// ends of those as soon as the process exits, dropping unread output.
		stdout, stdoutW, err := os.Pipe()
		if err != nil {
			return status.Errorf(codes.Internal, "failed to create stdout pipe: %v", err)
		}
		defer stdout.Close()
		stderr, stderrW, err := os.Pipe()
		if err != nil {
			_ = stdoutW.Close()
			return status.Errorf(codes.Internal, "failed to create stderr pipe: %v", err)
		}
		defer stderr.Close()
		cmd.Stdout = stdoutW
		cmd.Stderr = stderrW
		childEnds = []*os.File{stdoutW, stderrW}
		outputs = []execOutput{
			{name: "stdout", r: stdout, resp: stdoutResp},
			{name: "stderr", r: stderr, resp: stderrResp},
		}
	}

	execData := &ExecData{
		Id:       execId,
		Command:  details.Command,
		Created:  time.Now(),
		Deleted:  time.Time{},
		BytesIn:  0,
		BytesOut: 0,
	}
	s.grpcApi.ExecStore.Store(execData.Id, execData)

	// Start the command and check the output
	// When command execution fails, we write the error to stderr and return the exit code
	unlockCreation := s.grpcApi.procWatcher.LockForCreation()
	err = cmd.Start()
	for _, f := range childEnds {
		_ = f.Close()
	}
	if err != nil {
		if exitError, ok := err.(*exec.Error); ok {
			if exitError.Err == exec.ErrNotFound {
				exitCode = 127
			} else {
				exitCode = 126
			}
		} else {
			exitCode = 1
		}
		if sendErr := send(stderrResp([]byte(err.Error() + "\n"))); sendErr != nil {
			s.logger.Error().Msgf("Failed to send stderr: %v", sendErr)
		}
		if sendErr := send(&k8shelldv1.ExecResponse{Response: &k8shelldv1.ExecResponse_ExitCode{ExitCode: exitCode}}); sendErr != nil {
			s.logger.Error().Msgf("Failed to send exit code: %v", sendErr)
		}
		s.logger.Error().Msgf("Failed to start command: %v, exit-code: %d", err, exitCode)
		unlockCreation()
		return nil
	}

	processPID := cmd.Process.Pid

	// all good, add the PID to the ignore list not to be terminated as it will be orphaned
	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(processPID)
	unlockCreation()

	s.logger.Debug().Msgf("Executing command: %v, shell: %s, PID: %d", details, shell, processPID)

	if ptmx != nil {
		if details.Width > 0 && details.Height > 0 {
			if err := pty.Setsize(ptmx, &pty.Winsize{
				Rows: utils.ClampUint32ToUint16(details.Height),
				Cols: utils.ClampUint32ToUint16(details.Width),
			}); err != nil {
				s.logger.Error().Msgf("Failed to set PTY size: %v, PID=%d", err, processPID)
			}
		}
		if err := send(&k8shelldv1.ExecResponse{Response: &k8shelldv1.ExecResponse_Pty{Pty: tty.Name()}}); err != nil {
			s.logger.Debug().Msgf("Failed to send PTY name: %v, PID=%d", err, processPID)
		}
	}

	// -> Stream input, signals and resizes from client
	go func() {
		// A PTY cannot be half-closed, so stdin stays open until the PTY is
		// closed after the process exits.
		if ptmx == nil {
			defer stdin.Close()
			defer s.logger.Debug().Msgf("Closing stdin, PID=%d", processPID)
		}
		for {
			req, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					s.logger.Debug().Msgf("Client closed send (EOF), PID=%d", processPID)
				} else {
					s.logger.Debug().Msgf("Recv error: %v, PID=%d", err, processPID)
					if ptmx != nil {
						// Client is gone: hang up the terminal like a dropped connection would.
						_ = syscall.Kill(-processPID, syscall.SIGHUP)
					}
				}
				return
			}

			switch req.Request.(type) {
			case *k8shelldv1.ExecRequest_Input:
				data := req.GetInput()
				execData.BytesIn += uint64(len(data))
				if _, err := stdin.Write(data); err != nil {
					s.logger.Error().Msgf("Failed to write to stdin: %v, PID=%d", err, processPID)
				}
			case *k8shelldv1.ExecRequest_Resize:
				size := req.GetResize()
				if ptmx == nil {
					s.logger.Debug().Msgf("Ignoring resize for non-PTY exec, PID=%d", processPID)
					continue
				}
				s.logger.Debug().Msgf("Resizing exec PTY, cols: %d, rows: %d, PID=%d", size.GetWidth(), size.GetHeight(), processPID)
				if err := pty.Setsize(ptmx, &pty.Winsize{
					Rows: utils.ClampUint32ToUint16(size.GetHeight()),
					Cols: utils.ClampUint32ToUint16(size.GetWidth()),
				}); err != nil {
					s.logger.Error().Msgf("Failed to resize PTY: %v, PID=%d", err, processPID)
				}
			case *k8shelldv1.ExecRequest_Signal:
				s.logger.Debug().Msgf("Received signal %s, sending the signal to PID: %d", req.GetSignal(), processPID)
				signal, err := system.GetSignalValue(req.GetSignal())
				if err != nil {
					s.logger.Error().Msgf("Received invalid signal value: %v, PID=%d. Using SIGTERM.", err, processPID)
					signal = syscall.SIGTERM
				}

				// Send the signal to the process group
				err = syscall.Kill(-processPID, signal)
				if err != nil {
					err = syscall.Kill(processPID, signal)
					if err != nil {
						err = syscall.Kill(processPID, syscall.SIGKILL)
						if err != nil {
							s.logger.Error().Msgf("Failed to send kill process with PID %d: %v", processPID, err)
						}
					}
				}
				return
			}
		}
	}()

	// <- Stream stdout/stderr (or the PTY) to client
	outputsDone := make(chan struct{})
	var outputsWg sync.WaitGroup
	for _, out := range outputs {
		outputsWg.Add(1)
		go func(out execOutput) {
			defer outputsWg.Done()
			defer s.logger.Debug().Msgf("Closing %s, PID=%d", out.name, processPID)
			buf := make([]byte, 4096)
			for {
				n, err := out.r.Read(buf)
				if n > 0 {
					if sendErr := send(out.resp(buf[:n])); sendErr != nil {
						s.logger.Debug().Msgf("Failed to send %s data: %v, PID=%d", out.name, sendErr, processPID)
						return
					}
					execData.BytesOut += utils.SafeIntToUint64(n)
				}
				if err != nil {
					// A PTY master returns EIO once the slave side is closed; that is its EOF.
					if err == io.EOF || (ptmx != nil && errors.Is(err, syscall.EIO)) {
						s.logger.Debug().Msgf("%s closed by process, PID=%d", out.name, processPID)
					} else {
						s.logger.Debug().Msgf("Error reading %s: %v, PID=%d", out.name, err, processPID)
						if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
							if cmd.Process != nil {
								s.logger.Debug().Msgf("Sending SIGTERM to PID: %d due to %s error", processPID, out.name)
								if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
									s.logger.Error().Msgf("Failed to send SIGTERM to PID %d: %v", processPID, err)
								}
							}
						}
					}
					return
				}
			}
		}(out)
	}
	go func() {
		outputsWg.Wait()
		close(outputsDone)
	}()

	// Wait for the command to finish
	_ = cmd.Wait()

	// Drain what the process wrote before exiting so it lands before the exit
	// code. Background children may keep the output open, so don't wait
	// forever; the deferred Close of the read ends unblocks the readers.
	select {
	case <-outputsDone:
	case <-time.After(outputDrainTimeout):
		s.logger.Debug().Msgf("Output still open after process exit, PID=%d", processPID)
	}

	// Retrieve the exit code
	if cmd.ProcessState == nil {
		s.logger.Debug().Msgf("Command process state is nil, PID=%d", processPID)
		exitCode = 1
	} else {
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				signal := status.Signal()
				s.logger.Debug().Msgf("Process terminated by signal: %v, PID=%d", signal, processPID)
				exitCode = 128 + utils.SafeIntToInt32(int(signal))
			} else {
				exitCode = utils.SafeIntToInt32(status.ExitStatus())
			}
		} else {
			s.logger.Debug().Msgf("Unexpected process state type, PID=%d", processPID)
			exitCode = 1
		}
	}

	// Send the exit code to the client
	if sendErr := send(&k8shelldv1.ExecResponse{Response: &k8shelldv1.ExecResponse_ExitCode{ExitCode: exitCode}}); sendErr != nil {
		s.logger.Error().Msgf("Failed to send exit code: %v", sendErr)
	}
	s.logger.Debug().Msgf("Command execution complete: %v, PID=%d, exit-code=%d, bytes-in=%d, bytes-out=%d",
		details, processPID, exitCode, execData.BytesIn, execData.BytesOut)

	// Remove exec data from the store
	execData.Deleted = time.Now()

	return nil
}

// outputDrainTimeout bounds how long Exec waits for output after the process exits.
const outputDrainTimeout = time.Second

// execOutput is one output source of an exec process and how to wrap it for the client.
type execOutput struct {
	name string
	r    io.Reader
	resp func([]byte) *k8shelldv1.ExecResponse
}
