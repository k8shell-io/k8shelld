package api

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/logger"
	pb "github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// BufferedReadWriter is an interface that is used to read and write data with
// buffer size checking and stderr support.
type BufferedReadWriter interface {
	io.ReadWriter
	ReadBufferSize() (int, error)
	Stderr() io.Writer
}

// K8shelld is a client for the k8shelld gRPC service.
type K8shelld struct {
	client           *gapi.Client
	log              *zerolog.Logger
	systemClient     pb.SystemServiceClient
	shellClient      pb.ShellServiceClient
	execClient       pb.ExecServiceClient
	commandClient    pb.CommandServiceClient
	pfClient         pb.PortForwardServiceClient
	unixSocketClient pb.UnixSocketServiceClient
	app              pb.AppServiceClient
	counters         *ConnCounters
}

// ConnCounters holds counters for bytes sent and received.
type ConnCounters struct {
	inTotal  int64
	outTotal int64
}

// AddIn adds to the incoming byte counter.
func (c *ConnCounters) AddIn(n int) { atomic.AddInt64(&c.inTotal, int64(n)) }

// AddOut adds to the outgoing byte counter.
func (c *ConnCounters) AddOut(n int) { atomic.AddInt64(&c.outTotal, int64(n)) }

// Snapshot returns the current values of the incoming and outgoing byte counters.
func (c *ConnCounters) Snapshot() (in, out int64) {
	return atomic.LoadInt64(&c.inTotal), atomic.LoadInt64(&c.outTotal)
}

func NewClient(cfg gapi.ClientConfig, counters *ConnCounters) (*K8shelld, error) {
	gapiClient, err := gapi.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client: %w", err)
	}

	return &K8shelld{
		client:           gapiClient,
		log:              logger.NewLogger("k8shelld"),
		counters:         counters,
		systemClient:     pb.NewSystemServiceClient(gapiClient.Conn),
		shellClient:      pb.NewShellServiceClient(gapiClient.Conn),
		execClient:       pb.NewExecServiceClient(gapiClient.Conn),
		commandClient:    pb.NewCommandServiceClient(gapiClient.Conn),
		pfClient:         pb.NewPortForwardServiceClient(gapiClient.Conn),
		unixSocketClient: pb.NewUnixSocketServiceClient(gapiClient.Conn),
		app:              pb.NewAppServiceClient(gapiClient.Conn),
	}, nil
}

// Handshake performs a handshake with the k8shelld service to establish a session.
// The caller's identity JWT (user.UserToken) is sent so the server can verify it
// matches the workspace identity token.
func (c *K8shelld) Handshake(ctx context.Context, userToken string) (*pb.HandshakeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req := &pb.HandshakeRequest{
		UserToken: userToken,
	}

	return c.systemClient.Handshake(ctx, req)
}

func (c *K8shelld) GetSystemInfo(ctx context.Context) (*SystemInfo, error) {
	resp, err := c.systemClient.SystemInfo(ctx, &pb.SystemInfoRequest{})
	if err != nil {
		return nil, err
	}
	return ProtoToSystemInfo(resp), nil
}

// RunShell creates a PTY shell session over gRPC and bridges it with the BufferedReadWriter.
func (c *K8shelld) RunShell(ctx context.Context, rw BufferedReadWriter, sessionId string, envVars []string,
	width, height uint32, usePty bool, user string) error {
	token, ok := ctx.Value("token").(string)
	if !ok {
		return fmt.Errorf("missing token in context")
	}

	md := metadata.Pairs(
		"session-id", sessionId,
		"token", token,
	)
	ctx = metadata.NewOutgoingContext(ctx, md)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := c.shellClient.Shell(ctx)
	if err != nil {
		return fmt.Errorf("failed to create shell stream: %w", err)
	}

	startReq := &pb.ShellRequest{
		Request: &pb.ShellRequest_StartRequest{
			StartRequest: &pb.ShellStartRequest{
				CmdShell:   "/bin/sh",
				SetEnvVars: envVars,
				UsePty:     usePty,
				Width:      width,
				Height:     height,
				User:       user,
			},
		},
	}
	if err := stream.Send(startReq); err != nil {
		return fmt.Errorf("failed to send start request: %w", err)
	}

	errCh := make(chan error, 2)

	// writer goroutine (BufferedReadWriter -> gRPC)
	go func() {
		defer func() { _ = stream.CloseSend() }()

		buf := make([]byte, 32*1024)
		for {
			n, rerr := rw.Read(buf)
			if rerr != nil {
				if isEOFErrorOrCanceled(rerr) {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("reader read: %w", rerr)
				}
				return
			}
			if n == 0 {
				continue
			}
			if serr := stream.Send(&pb.ShellRequest{
				Request: &pb.ShellRequest_Data{
					Data: buf[:n],
				},
			}); serr != nil {
				errCh <- fmt.Errorf("grpc send: %w", serr)
				return
			}
			c.counters.AddIn(n)
		}
	}()

	// reader goroutine (gRPC -> BufferedReadWriter)
	go func() {
		for {
			resp, rerr := stream.Recv()
			if rerr != nil {
				if isEOFErrorOrCanceled(rerr) {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("grpc recv: %w", rerr)
				}
				return
			}

			switch r := resp.Response.(type) {
			case *pb.ShellResponse_Data:
				if _, werr := rw.Write(r.Data); werr != nil {
					errCh <- fmt.Errorf("writer write: %w", werr)
					return
				}
				c.counters.AddOut(len(r.Data))
			case *pb.ShellResponse_Terminate:
				if r.Terminate {
					errCh <- nil
					return
				}
			}
		}
	}()

	err = <-errCh

	// Drain the second result
	select {
	case <-errCh:
	default:
	}

	return err
}

// ResizeTerminal resizes the terminal
func (c *K8shelld) ResizeTerminal(ctx context.Context, sessionId string, width, height uint32) error {
	md := metadata.Pairs("session-id", sessionId)
	ctx = metadata.NewOutgoingContext(ctx, md)

	req := &pb.ResizeTerminalRequest{
		Width:  width,
		Height: height,
	}

	_, err := c.shellClient.ResizeTerminal(ctx, req)
	return err
}

// RunUnixSocket creates a Unix socket connection over gRPC and bridges it with the RW channel.
func (c *K8shelld) RunUnixSocket(ctx context.Context, upstream BufferedReadWriter, unixSocketId,
	socketPath string, mode string) error {

	token, ok := ctx.Value("token").(string)
	if !ok {
		return fmt.Errorf("missing token in context")
	}

	md := metadata.Pairs(
		"unixsocket-id", unixSocketId,
		"token", token,
	)
	ctx = metadata.NewOutgoingContext(ctx, md)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := c.unixSocketClient.UnixSocket(ctx)
	if err != nil {
		return fmt.Errorf("failed to create UnixSocket stream: %w", err)
	}

	modeVal, ok := pb.UnixSocketMode_value[mode]
	if !ok {
		return fmt.Errorf("invalid unix socket mode: %s", mode)
	}
	modeEnum := pb.UnixSocketMode(modeVal)

	startReq := &pb.UnixSocketRequest{
		Request: &pb.UnixSocketRequest_StartRequest{
			StartRequest: &pb.UnixSocketStartRequest{
				SocketPath: socketPath,
				Mode:       modeEnum,
			},
		},
	}
	if err := stream.Send(startReq); err != nil {
		return fmt.Errorf("failed to send UnixSocket start request: %w", err)
	}

	errCh := make(chan error, 2)

	// writer goroutine (upstream -> gRPC)
	go func() {
		defer func() { _ = stream.CloseSend() }()

		buf := make([]byte, 32*1024)
		for {
			size, err := upstream.ReadBufferSize()
			if err != nil {
				if isEOFErrorOrCanceled(err) {
					return
				}
				errCh <- fmt.Errorf("buffer check: %w", err)
				return
			}

			if size > 0 {
				n, rerr := upstream.Read(buf)
				if rerr != nil {
					if isEOFErrorOrCanceled(rerr) {
						errCh <- nil
					} else {
						errCh <- fmt.Errorf("upstream read: %w", rerr)
					}
					return
				}
				if n == 0 {
					continue
				}

				if serr := stream.Send(&pb.UnixSocketRequest{
					Request: &pb.UnixSocketRequest_Data{
						Data: buf[:n],
					},
				}); serr != nil {
					errCh <- fmt.Errorf("grpc send: %w", serr)
					return
				}
				c.counters.AddIn(n)
			} else {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
					// Continue checking
				}
			}
		}
	}()

	// reader goroutine (gRPC -> SSH)
	go func() {
		for {
			resp, rerr := stream.Recv()
			if rerr != nil {
				if isEOFErrorOrCanceled(rerr) {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("grpc recv: %w", rerr)
				}
				return
			}
			if _, werr := upstream.Write(resp.Data); werr != nil {
				errCh <- fmt.Errorf("upstream write: %w", werr)
				return
			}
			c.counters.AddOut(len(resp.Data))
		}
	}()

	err = <-errCh
	cancel()

	select {
	case <-errCh:
	default:
	}

	return err
}

// RunPortForward sets up a port forward over gRPC and bridges it with the upstream channel.
func (c *K8shelld) RunPortForward(ctx context.Context, upstream BufferedReadWriter, portForwardID, destinationIP string, destinationPort uint32) error {
	if destinationIP == "" {
		destinationIP = "localhost"
	}

	token, ok := ctx.Value("token").(string)
	if !ok {
		return fmt.Errorf("missing token in context")
	}

	md := metadata.Pairs(
		"portforward-id", portForwardID,
		"token", token,
	)
	ctx = metadata.NewOutgoingContext(ctx, md)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := c.pfClient.PortForward(ctx)
	if err != nil {
		return fmt.Errorf("failed to create PortForward stream: %w", err)
	}

	startReq := &pb.PortForwardRequest{
		Request: &pb.PortForwardRequest_Destination{
			Destination: &pb.Destination{
				Ip:   destinationIP,
				Port: destinationPort,
			},
		},
	}
	if err := stream.Send(startReq); err != nil {
		return fmt.Errorf("failed to send PortForward destination: %w", err)
	}

	errCh := make(chan error, 2)

	// writer goroutine (SSH -> gRPC)
	go func() {
		defer func() { _ = stream.CloseSend() }()

		buf := make([]byte, 32*1024)
		for {
			n, rerr := upstream.Read(buf)
			if rerr != nil {
				if isEOFErrorOrCanceled(rerr) {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("ssh read: %w", rerr)
				}
				return
			}
			if n == 0 {
				continue
			}
			if serr := stream.Send(&pb.PortForwardRequest{
				Request: &pb.PortForwardRequest_Data{
					Data: buf[:n],
				},
			}); serr != nil {
				errCh <- fmt.Errorf("grpc send: %w", serr)
				return
			}
			c.counters.AddIn(n)
		}
	}()

	// reader goroutine (gRPC -> SSH)
	go func() {
		for {
			resp, rerr := stream.Recv()
			if rerr != nil {
				if isEOFErrorOrCanceled(rerr) {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("grpc recv: %w", rerr)
				}
				return
			}
			if _, werr := upstream.Write(resp.Data); werr != nil {
				errCh <- fmt.Errorf("ssh write: %w", werr)
				return
			}
			c.counters.AddOut(len(resp.Data))
		}
	}()

	err = <-errCh

	// drain the second result
	select {
	case <-errCh:
	default:
	}
	return err
}

// RunExec executes a command in a remote shell over gRPC.
func (c *K8shelld) RunExec(ctx context.Context, upstream BufferedReadWriter, execID string,
	command string, shellBinary string, envVars []string, signalChan <-chan string) (int32, error) {

	token, ok := ctx.Value("token").(string)
	if !ok {
		return 1, fmt.Errorf("missing token in context")
	}

	md := metadata.Pairs("exec-id", execID, "token", token)
	ctx = metadata.NewOutgoingContext(ctx, md)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := c.execClient.Exec(ctx)
	if err != nil {
		return 1, fmt.Errorf("failed to create exec stream: %w", err)
	}

	startReq := &pb.ExecRequest{
		Request: &pb.ExecRequest_CommandDetails{
			CommandDetails: &pb.CommandDetails{
				Command:     command,
				ShellBinary: shellBinary,
				EnvVars:     envVars,
			},
		},
	}
	if err := stream.Send(startReq); err != nil {
		return 1, fmt.Errorf("failed to send exec command: %w", err)
	}

	var wg sync.WaitGroup
	var writerErr, readerErr error
	exitCodeCh := make(chan int32, 1)

	// helper to send signals
	sendSignal := func(signalName string) {
		signalReq := &pb.ExecRequest{
			Request: &pb.ExecRequest_Signal{
				Signal: signalName,
			},
		}
		if err := stream.Send(signalReq); err != nil {
			c.log.Error().Err(err).Msgf("Failed to send signal %s to exec process %s", signalName, execID)
		} else {
			c.log.Debug().Msgf("Successfully sent signal %s to exec process %s", signalName, execID)
		}
	}

	// writer goroutine (SSH -> gRPC)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			err = stream.CloseSend()
			if err != nil {
				c.log.Error().Err(err).Msgf("Failed to close exec stream for exec process %s", execID)
			}
		}()

		buf := make([]byte, 32*1024)
		for {
			select {
			case <-ctx.Done():
				return
			case signalName := <-signalChan:
				sendSignal(signalName)
			default:
			}

			size, err := upstream.ReadBufferSize()
			if err != nil {
				if isEOFErrorOrCanceled(err) {
					return
				}
				writerErr = fmt.Errorf("buffer check: %w", err)
				return
			}

			if size > 0 {
				n, rerr := upstream.Read(buf)
				if rerr != nil {
					if isEOFErrorOrCanceled(rerr) {
						return
					}
					writerErr = fmt.Errorf("reader read: %w", rerr)
					return
				}
				if n == 0 {
					continue
				}
				if serr := stream.Send(&pb.ExecRequest{
					Request: &pb.ExecRequest_Input{
						Input: buf[:n],
					},
				}); serr != nil {
					writerErr = fmt.Errorf("grpc send: %w", serr)
					return
				}
				c.counters.AddIn(n)
			} else {
				select {
				case <-ctx.Done():
					return
				case signalName := <-signalChan:
					sendSignal(signalName)
				case <-time.After(10 * time.Millisecond):
					// Continue checking
				}
			}
		}
	}()

	// reader goroutine (gRPC -> SSH)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		stderr := upstream.Stderr()

		for {
			resp, rerr := stream.Recv()
			if rerr != nil {
				if rerr == io.EOF {
					return
				}
				readerErr = fmt.Errorf("grpc recv: %w", rerr)
				return
			}

			switch r := resp.Response.(type) {
			case *pb.ExecResponse_Stdout:
				if _, err := upstream.Write(r.Stdout); err != nil {
					readerErr = fmt.Errorf("writer write: %w", err)
					return
				}
				c.counters.AddOut(len(r.Stdout))
			case *pb.ExecResponse_Stderr:
				if _, err := stderr.Write(r.Stderr); err != nil {
					readerErr = fmt.Errorf("ssh write: %w", err)
					return
				}
				c.counters.AddOut(len(r.Stderr))
			case *pb.ExecResponse_ExitCode:
				exitCodeCh <- r.ExitCode
				return
			default:
				readerErr = fmt.Errorf("unknown exec response type")
				return
			}
		}
	}()

	wg.Wait()

	var exitCode int32 = 0
	select {
	case exitCode = <-exitCodeCh:
	default:
		if writerErr != nil || readerErr != nil {
			exitCode = 1
		}
	}

	if writerErr != nil {
		c.log.Debug().Msgf("Exit code is %d, there is writeErr: %v", exitCode, writerErr)
		return exitCode, writerErr
	}
	if readerErr != nil {
		c.log.Debug().Msgf("Exit code is %d, there is readerErr: %v", exitCode, readerErr)
		return exitCode, readerErr
	}

	c.log.Debug().Msgf("Exit code is %d", exitCode)

	return exitCode, nil
}

// CommandHandler defines the signature for processing incoming commands.
// It receives the command string and returns the reply string (or an error).
type CommandHandler func(ctx context.Context, command string) (string, error)

// RunCommandProcessor starts a command processing loop.
func (k *K8shelld) RunCommandProcessor(ctx context.Context, handler CommandHandler) error {
	if handler == nil {
		return fmt.Errorf("nil command handler")
	}

	stream, err := k.commandClient.CommandListener(ctx)
	if err != nil {
		return fmt.Errorf("failed to create command listener stream: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		in, err := stream.Recv()
		if err != nil {
			if isEOFErrorOrCanceled(err) {
				return nil
			}
			return fmt.Errorf("failed to receive command from server: %w", err)
		}

		cmdID := in.GetCommandId()
		payload, ok := in.Payload.(*pb.CommandMessage_Command)
		if !ok {
			k.log.Warn().Str("command_id", cmdID).Msg("received non-command payload; ignoring")
			continue
		}

		cmdStr := payload.Command
		cmdCtx, cancel := context.WithCancel(ctx)
		replyStr, hErr := handler(cmdCtx, cmdStr)
		cancel()

		if hErr != nil {
			k.log.Error().Err(hErr).Str("command_id", cmdID).Msg("command handler returned error")
			if replyStr == "" {
				replyStr = fmt.Sprintf("error: %v", hErr)
			}
		}

		out := &pb.CommandMessage{
			CommandId: cmdID,
			Payload: &pb.CommandMessage_Reply{
				Reply: replyStr,
			},
		}

		if err := stream.Send(out); err != nil {
			return fmt.Errorf("failed to send command reply to server: %w", err)
		}
	}
}

// ListApps retrieves the list of applications from the k8shelld service
func (wc *K8shelld) ListApps(ctx context.Context) ([]*AppStatus, error) {
	resp, err := wc.app.ListApps(ctx, &pb.ListAppsRequest{})
	if err != nil {
		return nil, err
	}
	var apps []*AppStatus
	for _, appProto := range resp.Apps {
		apps = append(apps, AppStatusFromProto(appProto))
	}
	return apps, nil
}

func (wc *K8shelld) InstallApp(ctx context.Context, appName string) error {
	_, err := wc.app.InstallApp(ctx, &pb.InstallAppRequest{Name: appName})
	if err != nil {
		return err
	}
	return nil
}

// StartApp starts an application in the k8shelld service
func (wc *K8shelld) StartApp(ctx context.Context, appName string) error {
	_, err := wc.app.StartApp(ctx, &pb.StartAppRequest{Name: appName})
	return err
}

// StopApp stops an application in the k8shelld service
func (wc *K8shelld) StopApp(ctx context.Context, appName string) error {
	_, err := wc.app.StopApp(ctx, &pb.StopAppRequest{Name: appName})
	return err
}

// GetAppLogsStream retrieves a stream of logs for the specified application from the k8shelld service
// and returns it as an io.ReadCloser. The caller is responsible for closing the stream when done.
func (wc *K8shelld) GetAppLogsStream(ctx context.Context, appName string) (io.ReadCloser, error) {
	stream, err := wc.app.GetLogsStream(ctx, &pb.GetLogsStreamRequest{Name: appName})
	if err != nil {
		return nil, fmt.Errorf("failed to get app logs: %w", err)
	}

	pr, pw := io.Pipe()

	go func() {
		defer pw.Close()
		for {
			resp, err := stream.Recv()
			if err != nil {
				if isEOFErrorOrCanceled(err) {
					return
				}
				pw.CloseWithError(fmt.Errorf("error receiving log stream: %w", err))
				return
			}
			if _, werr := pw.Write([]byte(resp.GetLine())); werr != nil {
				pw.CloseWithError(fmt.Errorf("error writing to log pipe: %w", werr))
				return
			}
		}
	}()

	return pr, nil
}

func (wc *K8shelld) GetAppLogs(ctx context.Context, appName string) (string, error) {
	logs, err := wc.app.GetLogs(ctx, &pb.GetLogsRequest{Name: appName})
	if err != nil {
		return "", fmt.Errorf("failed to get app logs: %w", err)
	}
	return logs.GetLog(), nil
}

// Close closes the gRPC client connection
func (c *K8shelld) Close() error {
	return c.client.Close()
}

// *** Helper functions

// isEOFErrorOrCanceled reports whether the given error represents a clean EOF/stream close
// from the server, including the common gRPC-wrapped form
func isEOFErrorOrCanceled(err error) bool {
	if err == nil {
		return false
	}

	if err == io.EOF {
		return true
	}

	if st, ok := status.FromError(err); ok {
		if (st.Code() == codes.Unavailable && strings.Contains(st.Message(), "EOF")) || st.Code() == codes.Canceled {
			return true
		}
	}

	return false
}
