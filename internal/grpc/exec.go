package grpc

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
	"github.com/rs/zerolog"

	"github.com/google/shlex"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/internal/utils"
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

// ExecServiceServer is the service that handles the exec GRPC service server
type ExecServiceServer struct {
	grpcApi *GRPCService
	logger  *zerolog.Logger
	k8shelldpb.UnimplementedExecServiceServer
}

// NewExecServiceServer creates a new ExecServiceServer
func NewExecServiceServer(grpcapi *GRPCService) *ExecServiceServer {
	return &ExecServiceServer{
		grpcApi: grpcapi,
		logger:  logger.NewLogger("grpc-exec"),
	}
}

func parseCommand(cmdStr string) ([]string, error) {
	parts, err := shlex.Split(cmdStr)
	if err != nil {
		return nil, fmt.Errorf("error parsing command: %v", err)
	}
	return parts, nil
}

// Get the exec ID from the gRPC metadata "exec-id"
func (s *ExecServiceServer) GetExecID(ctx context.Context) (string, error) {
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

func (s *ExecServiceServer) Exec(stream k8shelldpb.ExecService_ExecServer) error {
	var cmd *exec.Cmd
	var stdin io.WriteCloser
	var stdout, stderr io.ReadCloser
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

	cmdReq, ok := req.Request.(*k8shelldpb.ExecRequest_CommandDetails)
	if !ok {
		return status.Errorf(codes.InvalidArgument, "invalid command request: %v", req)
	}

	args, err := parseCommand(cmdReq.CommandDetails.Command)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to parse command: %v", err)
	}
	if len(args) == 0 {
		return status.Errorf(codes.InvalidArgument, "invalid command: %v", args)
	}

	// Set the environment variables
	newEnv := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOME=") {
			newEnv = append(newEnv, fmt.Sprintf("HOME=%s", s.grpcApi.user.HomeDir))
			continue
		}
		newEnv = append(newEnv, e)
	}
	newEnv = append(newEnv, cmdReq.CommandDetails.EnvVars...)

	// Adjust the shell path if it is "sh" or "bash"
	if args[0] == "sh" || args[0] == "bash" {
		args[0] = "/bin/" + args[0]
	}

	// Create the command; use the shell if specified in the request
	if isValidShell(args[0]) {
		cmd = exec.Command(args[0], args[1:]...)
	} else {
		if cmdReq.CommandDetails.ShellBinary != "" && isValidShell(cmdReq.CommandDetails.ShellBinary) &&
			!strings.HasPrefix(cmdReq.CommandDetails.Command, cmdReq.CommandDetails.ShellBinary) {
			cmd = exec.Command(cmdReq.CommandDetails.ShellBinary, "-c", cmdReq.CommandDetails.Command)
		} else {
			cmd = exec.Command(args[0], args[1:]...)
		}
	}

	cmd.Env = newEnv
	cmd.Dir = s.grpcApi.user.HomeDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create a new process group
		Credential: &syscall.Credential{
			Uid: s.grpcApi.user.Uid,
			Gid: s.grpcApi.user.Gid,
		},
	}

	stdin, err = cmd.StdinPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create stdin pipe: %v", err)
	}
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create stdout pipe: %v", err)
	}
	stderr, err = cmd.StderrPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create stderr pipe: %v", err)
	}

	execData := &ExecData{
		Id:       execId,
		Command:  cmdReq.CommandDetails.Command,
		Created:  time.Now(),
		Deleted:  time.Time{},
		BytesIn:  0,
		BytesOut: 0,
	}
	s.grpcApi.ExecStore.Store(execData.Id, execData)

	// Start the command and check the output
	// When command execution fails, we write the error to stderr and return the exit code
	if err := cmd.Start(); err != nil {
		if exitError, ok := err.(*exec.Error); ok {
			if exitError.Err == exec.ErrNotFound {
				exitCode = 127
			} else {
				exitCode = 126
			}
		} else {
			exitCode = 1
		}
		if sendErr := stream.Send(&k8shelldpb.ExecResponse{
			Response: &k8shelldpb.ExecResponse_Stderr{Stderr: []byte(err.Error() + "\n")},
		}); sendErr != nil {
			s.logger.Error().Msgf("Failed to send stderr: %v", sendErr)
		}
		if sendErr := stream.Send(&k8shelldpb.ExecResponse{Response: &k8shelldpb.ExecResponse_ExitCode{ExitCode: exitCode}}); sendErr != nil {
			s.logger.Error().Msgf("Failed to send exit code: %v", sendErr)
		}
		s.logger.Error().Msgf("Failed to start command: %v, exit-code: %d", err, exitCode)
		return nil
	}

	processPID := cmd.Process.Pid

	// all good, add the PID to the ignore list not to be terminated as it will be orphaned
	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(processPID)

	s.logger.Debug().Msgf("Executing command: %v, PID: %d", cmdReq.CommandDetails, processPID)

	// Handle terminate request from client
	terminate := make(chan struct{})
	var terminateOnce sync.Once

	// -> Stream input and terminate from client
	go func() {
		defer stdin.Close()
		defer s.logger.Debug().Msgf("Closing stdin, PID=%d", processPID)
		for {
			req, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					s.logger.Debug().Msgf("Client closed send (EOF), PID=%d", processPID)
				} else {
					s.logger.Debug().Msgf("Recv error: %v, PID=%d", err, processPID)
				}
				terminateOnce.Do(func() { close(terminate) })
				return
			}

			switch req.Request.(type) {
			case *k8shelldpb.ExecRequest_Input:
				data := req.GetInput()
				execData.BytesIn += uint64(len(data))
				if _, err := stdin.Write(data); err != nil {
					s.logger.Error().Msgf("Failed to write to stdin: %v, PID=%d", err, processPID)
				}
			case *k8shelldpb.ExecRequest_Signal:
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
				terminateOnce.Do(func() { close(terminate) })
				return
			}
		}
	}()

	// <- Stream stdout to client
	go func() {
		defer stdout.Close()
		defer s.logger.Debug().Msgf("Closing stdout, PID=%d", processPID)
		buf := make([]byte, 4096)

		for {
			n, err := stdout.Read(buf)
			if err != nil {
				if err == io.EOF {
					s.logger.Debug().Msgf("Stdout closed by process, PID=%d", processPID)
				} else {
					s.logger.Debug().Msgf("Error reading stdout: %v, PID=%d", err, processPID)
					if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
						if cmd.Process != nil {
							s.logger.Debug().Msgf("Sending SIGTERM to PID: %d due to stdout error", processPID)
							if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
								s.logger.Error().Msgf("Failed to send SIGTERM to PID %d: %v", processPID, err)
							}
						}
					}
				}
				terminateOnce.Do(func() { close(terminate) })
				return
			}

			if n > 0 {
				streamErr := stream.Send(&k8shelldpb.ExecResponse{
					Response: &k8shelldpb.ExecResponse_Stdout{Stdout: buf[:n]},
				})
				if streamErr != nil {
					s.logger.Debug().Msgf("Failed to send stdout data: %v, PID=%d", streamErr, processPID)
					terminateOnce.Do(func() { close(terminate) })
					return
				} else {
					execData.BytesOut += utils.SafeIntToUint64(n)
				}
			}
		}
	}()

	// <- Stream stderr to client
	go func() {
		defer stderr.Close()
		defer s.logger.Debug().Msgf("Closing stderr, PID=%d", processPID)
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				if err == io.EOF {
					s.logger.Debug().Msgf("Stderr closed by process, PID=%d", processPID)
				} else {
					s.logger.Debug().Msgf("Error reading stderr: %v, PID=%d", err, processPID)
					if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
						if cmd.Process != nil {
							s.logger.Debug().Msgf("Sending SIGTERM to PID: %d due to stderr error", processPID)
							if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
								s.logger.Error().Msgf("Failed to send SIGTERM to PID %d: %v", processPID, err)
							}
						}
					}
				}
				terminateOnce.Do(func() { close(terminate) })
				return
			}

			if n > 0 {
				if sendErr := stream.Send(&k8shelldpb.ExecResponse{
					Response: &k8shelldpb.ExecResponse_Stderr{Stderr: buf[:n]},
				}); sendErr != nil {
					s.logger.Error().Msgf("Failed to send stderr data: %v, PID=%d", sendErr, processPID)
				}
				execData.BytesOut += utils.SafeIntToUint64(n)
			}
		}
	}()

	// Wait for the command to finish
	_ = cmd.Wait()

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
	if sendErr := stream.Send(&k8shelldpb.ExecResponse{Response: &k8shelldpb.ExecResponse_ExitCode{ExitCode: exitCode}}); sendErr != nil {
		s.logger.Error().Msgf("Failed to send exit code: %v", sendErr)
	}
	s.logger.Debug().Msgf("Command execution complete: %v, PID=%d, exit-code=%d, bytes-in=%d, bytes-out=%d",
		cmdReq.CommandDetails, processPID, exitCode, execData.BytesIn, execData.BytesOut)

	// Remove exec data from the store
	execData.Deleted = time.Now()

	return nil
}
