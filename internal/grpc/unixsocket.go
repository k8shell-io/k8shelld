package grpc

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/utils"
	"github.com/rs/zerolog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type unixSocketData struct {
	Id         string
	socketPath string
	Created    time.Time
	Deleted    time.Time
	BytesIn    uint64
	BytesOut   uint64
	Mode       string
	// ShellPid is the PID of the shell session that owns this socket.
	// When non-zero, only processes descended from this PID are allowed
	// to connect (enforced via SO_PEERCRED after Accept).
	ShellPid int
}

// UnixSocketHandler is the service that handles the shell GRPC service server
type UnixSocketHandler struct {
	grpcApi *GRPCService
	logger  *zerolog.Logger
}

// newUnixSocketHandler creates a new UnixSocketHandler
func newUnixSocketHandler(grpcapi *GRPCService) *UnixSocketHandler {
	return &UnixSocketHandler{
		grpcApi: grpcapi,
		logger:  logger.NewLogger("grpc-unixsocket"),
	}
}

// Get the unix socket ID from the gRPC metadata "unixsocket-id"
func (s *UnixSocketHandler) GetUnixSocketID(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "missing metadata")
	}

	data := md.Get("unixsocket-id")
	if len(data) == 0 {
		return "", status.Errorf(codes.InvalidArgument, "missing unixsocket-id")
	}

	return data[0], nil
}

// Get the unix socket data from the store. It uses the unix socket ID retrieved from the metadata
func (s *UnixSocketHandler) GetUnixSocketData(ctx context.Context) (*SessionData, error) {
	sid, err := s.GetUnixSocketID(ctx)
	if err != nil {
		return nil, err
	}
	value, ok := s.grpcApi.UnixSocketStore.Load(sid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "unixsocket-id %s not found", sid)
	}
	return value.(*SessionData), nil
}

func (s *UnixSocketHandler) UnixSocket(stream grpc.BidiStreamingServer[k8shelldv1.UnixSocketRequest, k8shelldv1.UnixSocketResponse]) error {
	uxid, err := s.GetUnixSocketID(stream.Context())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to get unixsocket-id: %v", err)
	}

	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive request: %v", err)
	}

	start, ok := req.Request.(*k8shelldv1.UnixSocketRequest_StartRequest)
	if !ok {
		return status.Errorf(codes.InvalidArgument,
			"invalid unix socket request, expected unix socket start request: %v", req)
	}

	switch start.StartRequest.Mode {
	case k8shelldv1.UnixSocketMode_UNIX_SOCKET_MODE_LISTEN:
		return s.startListenerAndBridge(uxid, start.StartRequest.SocketPath, stream)
	case k8shelldv1.UnixSocketMode_UNIX_SOCKET_MODE_DIAL:
		return s.dialAndBridge(uxid, start.StartRequest.SocketPath, stream)
	default:
		return status.Errorf(codes.InvalidArgument, "invalid unix socket mode")
	}
}

// startListenerAndBridge starts a Unix socket listener and bridges gRPC <-> conn
func (s *UnixSocketHandler) startListenerAndBridge(uxid, socketPath string,
	stream grpc.BidiStreamingServer[k8shelldv1.UnixSocketRequest, k8shelldv1.UnixSocketResponse]) error {

	unixsocket := &unixSocketData{
		Id:         uxid,
		socketPath: socketPath,
		Created:    time.Now(),
		Mode:       "listen",
	}

	// Associate the socket with the owning shell session so that we can
	// enforce process-ancestry checks on Accept.  Shell and unix-socket IDs
	// share the same base (e.g. "sh-p98l6-78-2i1" and "ux-p98l6-78-2i2"
	// both have base "p98l6-78-2i").  We match on that base to find the
	// right SessionData in SessionStore.
	if sess := s.findShellSessionByBase(uxid); sess != nil && sess.Pid > 0 {
		unixsocket.ShellPid = sess.Pid
	}

	if _, err := os.Lstat(unixsocket.socketPath); err == nil {
		_ = os.Remove(unixsocket.socketPath)
	}

	uxListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: unixsocket.socketPath, Net: "unix"})
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create Unix socket listener: %v", err)
	}

	if err := os.Chown(unixsocket.socketPath, int(s.grpcApi.user.GetUID()), int(s.grpcApi.user.GetGID())); err != nil {
		return status.Errorf(codes.Internal, "failed to chown socket: %v", err)
	}
	if err := os.Chmod(unixsocket.socketPath, 0700); err != nil {
		return status.Errorf(codes.Internal, "failed to chmod socket: %v", err)
	}

	s.logger.Info().Msgf("unix-socket listener started, id=%s, path=%s", unixsocket.Id, unixsocket.socketPath)
	s.grpcApi.UnixSocketStore.Store(unixsocket.Id, unixsocket)
	defer func() {
		unixsocket.Deleted = time.Now()
		_ = uxListener.Close()
		s.logger.Info().Msgf("Unix socket stream (listen mode) ended, id=%s, path=%s", unixsocket.Id, unixsocket.socketPath)
	}()

	return s.communicate(uxListener, unixsocket, stream)
}

// dialAndBridge dials a Unix socket and bridges gRPC <-> conn
func (s *UnixSocketHandler) dialAndBridge(uxid, socketPath string,
	stream grpc.BidiStreamingServer[k8shelldv1.UnixSocketRequest, k8shelldv1.UnixSocketResponse]) error {

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to dial unix socket %s: %v", socketPath, err)
	}

	unixsocket := &unixSocketData{
		Id:         uxid,
		socketPath: socketPath,
		Created:    time.Now(),
		Mode:       "dial",
	}

	s.logger.Info().Msgf("unix-socket dial connected, id=%s, target=%s", uxid, socketPath)
	s.grpcApi.UnixSocketStore.Store(unixsocket.Id, unixsocket)
	defer func() {
		unixsocket.Deleted = time.Now()
		_ = conn.Close()
		s.logger.Info().Msgf("Unix socket stream (dial mode) ended, id=%s, path=%s", unixsocket.Id, unixsocket.socketPath)
	}()

	errCh := make(chan error, 2)

	// gRPC -> Unix
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("recv error: %w", err)
				}
				return
			}
			data := req.GetData()
			if len(data) == 0 {
				continue
			}
			if _, werr := conn.Write(data); werr != nil {
				errCh <- fmt.Errorf("write to unix: %w", werr)
				return
			}
			unixsocket.BytesIn += uint64(len(data))
		}
	}()

	// Unix -> gRPC
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := conn.Read(buf)
			if rerr != nil {
				if rerr == io.EOF {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("read from unix: %w", rerr)
				}
				return
			}
			if n == 0 {
				continue
			}
			if serr := stream.Send(&k8shelldv1.UnixSocketResponse{Data: buf[:n]}); serr != nil {
				errCh <- fmt.Errorf("send to stream: %w", serr)
				return
			}
			unixsocket.BytesOut += utils.SafeIntToUint64(n)
		}
	}()

	// Wait for either side to end
	if err := <-errCh; err != nil {
		s.logger.Error().Msgf("unix-socket dial bridge error: %v", err)
		return status.Errorf(codes.Internal, "unix-socket dial bridge error: %v", err)
	}
	return nil
}

func (s *UnixSocketHandler) communicate(uxListener *net.UnixListener, unixsocket *unixSocketData,
	stream grpc.BidiStreamingServer[k8shelldv1.UnixSocketRequest, k8shelldv1.UnixSocketResponse]) error {
	buf := make([]byte, 1024)
	stop := make(chan struct{})
	defer close(stop)

	mu := sync.Mutex{}
	conn := net.Conn(nil)

	// Goroutine to read from unixConn and send to gRPC stream
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				// Accept the connection from the client
				s.logger.Info().Msgf("Waiting for the client connection")
				var err error
				conn, err = uxListener.Accept()
				if err != nil {
					if err == io.EOF {
						s.logger.Info().Msg("Unix listener closed")
					} else {
						s.logger.Error().Msgf("Failed to accept connection: %v", err)
					}
					break
				}

				// Enforce process-ancestry restriction: the connecting process must
				// be a descendant of the shell that owns this socket.
				{
					sess := s.findShellSessionByBase(unixsocket.Id)
					ownerPid := 0
					if sess != nil {
						ownerPid = sess.Pid
					} else {
						ownerPid = unixsocket.ShellPid
					}

					var rejectReason string
					if sess != nil && ownerPid == 0 {
						rejectReason = fmt.Sprintf(
							"unix-socket %s: rejected connection (owning shell not yet started)",
							unixsocket.socketPath)
					} else if ownerPid > 0 {
						if uc, ok := conn.(*net.UnixConn); ok {
							pid, perr := peerPid(uc)
							if perr != nil || !isProcDescendant(pid, ownerPid) {
								rejectReason = fmt.Sprintf(
									"unix-socket %s: rejected connection from PID %d (not descendant of shell PID %d)",
									unixsocket.socketPath, pid, ownerPid)
							}
						}
					}

					if rejectReason != "" {
						s.logger.Warn().Msg(rejectReason)
						conn.Close()
						mu.Lock()
						conn = nil
						mu.Unlock()
						break
					}
				}

				s.logger.Info().Msg("Client connected")

				for {
					mu.Lock()
					c := conn
					mu.Unlock()

					if c == nil {
						break
					}

					n, err := c.Read(buf)
					if err != nil {
						if err == io.EOF {
							s.logger.Info().Msg("Client connection closed")
						} else {
							s.logger.Error().Msgf("Error reading from unix socket: %v", err)
						}
						break
					} else {
						if err := stream.Send(&k8shelldv1.UnixSocketResponse{
							Data: buf[:n],
						}); err != nil {
							s.logger.Error().Msgf("Failed to send data to the client: %v", err)
							break
						}
						unixsocket.BytesOut += utils.SafeIntToUint64(n)
					}
				}

				// Close the connection
				mu.Lock()
				conn.Close()
				conn = nil
				mu.Unlock()
			}
		}
	}()

	// Main loop to read from gRPC stream and write to unixConn
	for {
		req, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				s.logger.Info().Msg("Client closed the stream")
			} else {
				s.logger.Error().Msgf("Failed to receive: %v", err)
			}
			break
		}

		mu.Lock()
		c := conn
		mu.Unlock()

		if c != nil {
			data := req.GetData()
			if _, err := c.Write(data); err != nil {
				s.logger.Error().Msgf("Failed to write data to the unix socket: %v", err)
				break
			}
			unixsocket.BytesIn += utils.SafeIntToUint64(len(data))
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	s.logger.Info().Msg("Communication ended")
	uxListener.Close()
	return nil
}

// findShellSessionByBase searches SessionStore for a shell session whose ID
// shares the same base as the given unix-socket ID.  Both ID types use the
// scheme "{prefix}-{proxyID}-{pid}-{random2chars}{counter}" where the base
// is everything after the stream type prefix with trailing counter digits stripped.
func (s *UnixSocketHandler) findShellSessionByBase(uxid string) *SessionData {
	base := streamBase(uxid)
	if base == "" {
		return nil
	}
	var found *SessionData
	s.grpcApi.SessionStore.Range(func(key, value any) bool {
		sess, ok := value.(*SessionData)
		if !ok {
			return true
		}
		if streamBase(sess.Id) == base {
			found = sess
			return false // stop
		}
		return true
	})
	return found
}

// streamBase extracts the shared base from a stream ID generated with the
// pattern "{type}-{proxyID}-{pid}-{random2chars}{counter}", e.g.:
//
//	"sh-p98l6-78-2i1" → "p98l6-78-2i"
//	"ux-p98l6-78-2i2" → "p98l6-78-2i"
//
// The random 2 chars can contain digits, so we cannot reliably strip trailing
// digits.  Instead we split on "-" and take exactly the first 2 characters of
// the last segment as the random part, discarding the trailing counter.
func streamBase(id string) string {
	parts := strings.SplitN(id, "-", 4)
	// parts: [type, proxyID, pid, random2chars+counter]
	if len(parts) != 4 || len(parts[3]) < 2 {
		return ""
	}
	return parts[1] + "-" + parts[2] + "-" + parts[3][:2]
}

// peerPid returns the PID of the process on the other end of a Unix socket
// connection using SO_PEERCRED.
func peerPid(conn *net.UnixConn) (int, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var credErr error
	if ctrlErr := rawConn.Control(func(fd uintptr) {
		ucred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		pid = int(ucred.Pid)
	}); ctrlErr != nil {
		return 0, ctrlErr
	}
	return pid, credErr
}

// isProcDescendant returns true when childPid is the same as ancestorPid or
// is a descendant of it in the /proc process tree.  The walk is capped at 64
// levels to guard against cycles in unusual namespaces.
func isProcDescendant(childPid, ancestorPid int) bool {
	const maxDepth = 64
	pid := childPid
	for i := 0; i < maxDepth; i++ {
		if pid == ancestorPid {
			return true
		}
		if pid <= 1 {
			return false
		}
		ppid, err := procParentPid(pid)
		if err != nil || ppid == pid {
			return false
		}
		pid = ppid
	}
	return false
}

// procParentPid reads the PPid field from /proc/<pid>/status.
func procParentPid(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				break
			}
			return strconv.Atoi(fields[1])
		}
	}
	return 0, fmt.Errorf("PPid not found in /proc/%d/status", pid)
}
