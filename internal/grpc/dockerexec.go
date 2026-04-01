package grpc

import (
	"context"
	"fmt"
	"io"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/utils"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newDockerClient creates a Docker client using environment variables or defaults.
func newDockerClient() (*dockerclient.Client, error) {
	return dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
}

// resolveContainerID maps a container_ref (container ID, name, or Compose service name)
// to a running container ID.
func resolveContainerID(ctx context.Context, cli *dockerclient.Client, ref string) (string, error) {
	info, err := cli.ContainerInspect(ctx, ref)
	if err == nil {
		return info.ID, nil
	}
	if !dockerclient.IsErrNotFound(err) {
		return "", fmt.Errorf("docker inspect %q: %w", ref, err)
	}

	// Fall back to Compose service label lookup.
	fa := filters.NewArgs()
	fa.Add("label", "com.docker.compose.service="+ref)
	fa.Add("status", "running")
	list, err := cli.ContainerList(ctx, dockercontainer.ListOptions{Filters: fa})
	if err != nil {
		return "", fmt.Errorf("docker list containers for service %q: %w", ref, err)
	}
	if len(list) == 0 {
		return "", fmt.Errorf("no running container found for ref %q", ref)
	}
	return list[0].ID, nil
}

// startDockerExec creates and attaches a Docker exec instance for the given container.
func startDockerExec(ctx context.Context, cli *dockerclient.Client,
	containerID, shell, user string, envVars []string, usePty bool,
) (execID string, hijack dockertypes.HijackedResponse, err error) {
	created, err := cli.ContainerExecCreate(ctx, containerID, dockercontainer.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: !usePty, // with Tty=true Docker merges stderr into stdout
		Tty:          usePty,
		User:         user,
		Env:          envVars,
		Cmd:          []string{shell},
	})
	if err != nil {
		return "", dockertypes.HijackedResponse{}, fmt.Errorf("exec create: %w", err)
	}

	hijack, err = cli.ContainerExecAttach(ctx, created.ID, dockercontainer.ExecAttachOptions{Tty: usePty})
	if err != nil {
		return "", dockertypes.HijackedResponse{}, fmt.Errorf("exec attach: %w", err)
	}
	return created.ID, hijack, nil
}

// runDockerShell sets up and runs a full Docker exec shell session over gRPC.
func (s *ShellServiceServer) runDockerShell(stream k8shelldv1.ShellService_ShellServer,
	sessionId string, req *k8shelldv1.ShellStartRequest,
) error {
	shell := req.CmdShell
	if shell == "" {
		shell = "/bin/sh"
	}

	session := &SessionData{
		Id:       sessionId,
		user:     models.ShellUser{Username: req.AsUser},
		CmdShell: shell,
		Pid:      -1,
		Created:  time.Now(),
		Deleted:  time.Time{},
	}

	cli, err := newDockerClient()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to connect to Docker: %v", err)
	}

	containerID, err := resolveContainerID(stream.Context(), cli, req.ContainerRef)
	if err != nil {
		_ = cli.Close()
		return status.Errorf(codes.NotFound, "failed to resolve container %q: %v", req.ContainerRef, err)
	}

	execID, hijack, err := startDockerExec(stream.Context(), cli, containerID,
		shell, req.AsUser, req.SetEnvVars, req.UsePty)
	if err != nil {
		_ = cli.Close()
		return status.Errorf(codes.Internal, "failed to start docker exec: %v", err)
	}

	session.DockerExecID = execID
	session.dockerClient = cli
	s.grpcApi.SessionStore.Store(session.Id, session)

	defer func() {
		s.cleanUpSession(session)
		session.Deleted = time.Now()
		s.logger.Info().Msgf("Docker shell session %s ended (container=%s)", sessionId, req.ContainerRef)
	}()

	s.logger.Info().Msgf("Starting Docker shell session %s container=%s pty=%v",
		sessionId, req.ContainerRef, req.UsePty)

	if req.UsePty {
		err = s.handleDockerPtySession(s.logger, session, stream, hijack, req.Width, req.Height)
	} else {
		err = s.handleDockerNonPtySession(s.logger, session, stream, hijack)
	}
	if err != nil {
		return fmt.Errorf("docker shell session %s: %w", sessionId, err)
	}
	return nil
}

// resizeDockerTerminal resizes the TTY of a running Docker exec session.
func (s *ShellServiceServer) resizeDockerTerminal(ctx context.Context, session *SessionData, width, height uint32) error {
	cli := session.dockerClient.(*dockerclient.Client)
	return cli.ContainerExecResize(ctx, session.DockerExecID, dockercontainer.ResizeOptions{
		Width:  uint(width),
		Height: uint(height),
	})
}

// handleDockerPtySession streams a PTY-based Docker exec session over gRPC.
func (s *ShellServiceServer) handleDockerPtySession(
	logger *zerolog.Logger, session *SessionData,
	stream k8shelldv1.ShellService_ShellServer,
	hijack dockertypes.HijackedResponse,
	width, height uint32,
) error {
	defer hijack.Close()

	if width > 0 && height > 0 {
		if resizeErr := s.resizeDockerTerminal(stream.Context(), session, width, height); resizeErr != nil {
			logger.Error().Msgf("Failed to set initial docker terminal size: %v", resizeErr)
		}
	}

	_ = stream.Send(&k8shelldv1.ShellResponse{
		Response: &k8shelldv1.ShellResponse_StartResponse{
			StartResponse: &k8shelldv1.ShellStartResponse{},
		},
	})

	if s.grpcApi.Config.Splash != "" {
		_ = stream.Send(&k8shelldv1.ShellResponse{
			Response: &k8shelldv1.ShellResponse_Data{
				Data: []byte("\n\r" + s.grpcApi.Config.ExpandSplash(s.grpcApi.user, session.user.Username) + "\n\r"),
			},
		})
	}

	ctx := stream.Context()
	reqCh := make(chan *k8shelldv1.ShellRequest, 8)
	recvErrCh := make(chan error, 1)
	readDone := make(chan struct{})

	// Docker -> client
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := hijack.Reader.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&k8shelldv1.ShellResponse{
					Response: &k8shelldv1.ShellResponse_Data{Data: append([]byte(nil), buf[:n]...)},
				}); sendErr != nil {
					recvErrCh <- sendErr
					return
				}
				session.BytesOut += utils.SafeIntToUint64(n)
			}
			if err != nil {
				close(readDone)
				return
			}
		}
	}()

	// client -> Docker
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
			return nil
		case <-readDone:
			return nil
		case err := <-recvErrCh:
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("stream error: %w", err)
		case req, ok := <-reqCh:
			if !ok {
				return nil
			}
			if data := req.GetData(); data != nil {
				if _, werr := hijack.Conn.Write(data); werr != nil {
					return fmt.Errorf("docker stdin write: %w", werr)
				}
				session.BytesIn += uint64(len(data))
			}
		}
	}
}

// handleDockerNonPtySession streams a non-PTY Docker exec session over gRPC.
// Docker multiplexes stdout and stderr; stdcopy demuxes them both to the gRPC stream.
func (s *ShellServiceServer) handleDockerNonPtySession(
	logger *zerolog.Logger, session *SessionData,
	stream k8shelldv1.ShellService_ShellServer,
	hijack dockertypes.HijackedResponse,
) error {
	defer hijack.Close()

	_ = stream.Send(&k8shelldv1.ShellResponse{
		Response: &k8shelldv1.ShellResponse_StartResponse{
			StartResponse: &k8shelldv1.ShellStartResponse{},
		},
	})

	ctx := stream.Context()
	reqCh := make(chan *k8shelldv1.ShellRequest, 8)
	recvErrCh := make(chan error, 1)
	outDone := make(chan struct{})

	// Docker multiplexed output -> client
	go func() {
		sw := &streamWriter{stream: stream}
		if _, err := stdcopy.StdCopy(sw, sw, hijack.Reader); err != nil && err != io.EOF {
			logger.Error().Msgf("docker stdcopy error (session %s): %v", session.Id, err)
		}
		close(outDone)
	}()

	// client -> Docker
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
			return nil
		case <-outDone:
			return nil
		case err := <-recvErrCh:
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("stream error: %w", err)
		case req, ok := <-reqCh:
			if !ok {
				continue
			}
			if data := req.GetData(); data != nil {
				if _, werr := hijack.Conn.Write(data); werr != nil {
					return fmt.Errorf("docker stdin write: %w", werr)
				}
				session.BytesIn += uint64(len(data))
			}
		}
	}
}
