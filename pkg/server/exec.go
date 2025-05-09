package server

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

	"github.com/k8shell-io/k8shelld/grpc/generated-go/k8shelldpb"

	"github.com/google/shlex"
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

func parseCommand(cmdStr string) ([]string, error) {
	parts, err := shlex.Split(cmdStr)
	if err != nil {
		return nil, fmt.Errorf("Error parsing command: %v", err)
	}
	return parts, nil
}

// Get the exec ID from the gRPC metadata "exec-id"
func (s *RemoteOSServiceServer) GetExecID(ctx context.Context) (string, error) {
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

func (s *RemoteOSServiceServer) Exec(stream k8shelldpb.RemoteOSService_ExecServer) error {
	var logger = NewLogger("grpc-exec")
	var cmd *exec.Cmd
	var stdin io.WriteCloser
	var stdout, stderr io.ReadCloser
	var exitCode int32 = -1

	execId, err := s.GetExecID(stream.Context())
	if err != nil {
		return logger.ErrorR("Failed to get exec ID: %v", err)
	}

	_, ok := s.grpcApi.execStore.Load(execId)
	if ok {
		return status.Errorf(codes.AlreadyExists, "exec-id %s already exists", execId)
	}

	// The first request is the command
	req, err := stream.Recv()
	if err != nil {
		return logger.ErrorR("Failed to receive command: %v", err)
	}

	cmdReq, ok := req.Request.(*k8shelldpb.ExecRequest_CommandDetails)
	if !ok {
		return logger.ErrorR("Invalid command request: %v", req)
	}

	args, err := parseCommand(cmdReq.CommandDetails.Command)
	if err != nil {
		return logger.ErrorR("Failed to parse command: %v", err)
	}
	if len(args) == 0 {
		return logger.ErrorR("Invalid command: %v", args)
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
	newEnv = append(newEnv, cmdReq.CommandDetails.SetEnvVars...)

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
			Uid: uint32(s.grpcApi.user.Uid),
			Gid: uint32(s.grpcApi.user.Gid),
		},
	}

	stdin, err = cmd.StdinPipe()
	if err != nil {
		return logger.ErrorR("Failed to create stdin pipe: %v", err)
	}
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		return logger.ErrorR("Failed to create stdout pipe: %v", err)
	}
	stderr, err = cmd.StderrPipe()
	if err != nil {
		return logger.ErrorR("Failed to create stderr pipe: %v", err)
	}

	execData := &ExecData{
		Id:       execId,
		Command:  cmdReq.CommandDetails.Command,
		Created:  time.Now(),
		Deleted:  time.Time{},
		BytesIn:  0,
		BytesOut: 0,
	}
	s.grpcApi.execStore.Store(execData.Id, execData)

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
		stream.Send(&k8shelldpb.ExecResponse{
			Response: &k8shelldpb.ExecResponse_Stderr{Stderr: []byte(err.Error() + "\n")},
		})
		stream.Send(&k8shelldpb.ExecResponse{Response: &k8shelldpb.ExecResponse_ExitCode{ExitCode: exitCode}})
		logger.Error("Failed to start command: %v, exit-code: %d", err, exitCode)
		return nil
	}

	processPID := cmd.Process.Pid

	// all good, add the PID to the ignore list not to be terminated as it will be orphaned
	AddPIDIgnoreTerminate(processPID)

	logger.Debug("Executing command: %v, PID: %d", cmdReq.CommandDetails, processPID)

	// Handle terminate request from client
	terminate := make(chan struct{})
	var terminateOnce sync.Once

	// -> Stream input and terminate from client
	go func() {
		defer stdin.Close()
		defer logger.Debug("Closing stdin, PID=%d", processPID)
		for {
			req, err := stream.Recv()
			if err != nil {
				if err != io.EOF {
					logger.Debug("Error receiving stream: %v, PID=%d", err, processPID)
				}
				cmd.Process.Signal(syscall.SIGTERM)
				terminateOnce.Do(func() { close(terminate) })
				return
			}

			switch req.Request.(type) {
			case *k8shelldpb.ExecRequest_Input:
				data := req.GetInput()
				execData.BytesIn += uint64(len(data))
				stdin.Write(data)
			case *k8shelldpb.ExecRequest_Signal:
				logger.Debug("Received signal %s, sending the signal to PID: %d", req.GetSignal(), processPID)
				signal, err := getSignalValue(req.GetSignal())
				if err != nil {
					logger.Error("Received invalid signal value: %v, PID=%d. Using SIGTERM.", err, processPID)
					signal = syscall.SIGTERM
				}

				// Send the signal to the process group
				err = syscall.Kill(-processPID, signal)
				if err != nil {
					err = syscall.Kill(processPID, signal)
					if err != nil {
						err = syscall.Kill(processPID, syscall.SIGKILL)
						if err != nil {
							logger.Error("Failed to send kill process with PID %d: %v", processPID, err)
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
		defer logger.Debug("Closing stdout, PID=%d", processPID)
		buf := make([]byte, 4096)

		for {
			n, err := stdout.Read(buf)
			if err != nil {
				if err == io.EOF {
					logger.Debug("Stdout closed by process, PID=%d", processPID)
				} else {
					logger.Debug("Error reading stdout: %v, PID=%d", err, processPID)
					if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
						if cmd.Process != nil {
							logger.Debug("Sending SIGTERM to PID: %d due to stdout error", processPID)
							cmd.Process.Signal(syscall.SIGTERM)
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
					logger.Debug("Failed to send stdout data: %v, PID=%d", streamErr, processPID)
					terminateOnce.Do(func() { close(terminate) })
					return
				} else {
					execData.BytesOut += uint64(n)
				}
			}
		}
	}()

	// <- Stream stderr to client
	go func() {
		defer stderr.Close()
		defer logger.Debug("Closing stderr, PID=%d", processPID)
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				if err == io.EOF {
					logger.Debug("Stderr closed by process, PID=%d", processPID)
				} else {
					logger.Debug("Error reading stderr: %v, PID=%d", err, processPID)
					if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
						if cmd.Process != nil {
							logger.Debug("Sending SIGTERM to PID: %d due to stderr error", processPID)
							cmd.Process.Signal(syscall.SIGTERM)
						}
					}
				}
				terminateOnce.Do(func() { close(terminate) })
				return
			}

			if n > 0 {
				stream.Send(&k8shelldpb.ExecResponse{
					Response: &k8shelldpb.ExecResponse_Stderr{Stderr: buf[:n]},
				})
			} else {
				execData.BytesOut += uint64(n)
			}
		}
	}()

	// Wait for the command to finish
	cmd.Wait()

	// Retrieve the exit code
	if cmd.ProcessState == nil {
		logger.Debug("Command process state is nil, PID=%d", processPID)
		exitCode = 1
	} else {
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				signal := status.Signal()
				logger.Debug("Process terminated by signal: %v, PID=%d", signal, processPID)
				exitCode = 128 + int32(signal)
			} else {
				exitCode = int32(status.ExitStatus())
			}
		} else {
			logger.Debug("Unexpected process state type, PID=%d", processPID)
			exitCode = 1
		}
	}

	// Send the exit code to the client
	stream.Send(&k8shelldpb.ExecResponse{Response: &k8shelldpb.ExecResponse_ExitCode{ExitCode: exitCode}})
	logger.Debug("Command execution complete: %v, PID=%d, exit-code=%d, bytes-in=%d, bytes-out=%d",
		cmdReq.CommandDetails, processPID, exitCode, execData.BytesIn, execData.BytesOut)

	// Remove exec data from the store
	execData.Deleted = time.Now()

	return nil
}
