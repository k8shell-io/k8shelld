package grpc

// detachable.go — detach/reattach for PTY shell sessions.
//
// All PTY sessions have a ring buffer and a session-owned PTY read loop.
// By default, closing the gRPC stream or REST connection destroys the session,
//
// Detach: `kbox detach` inside the session which calls
// POST /api/v1/shells/{id}/detach - DetachShellSession - closes the detachRequested
// channel. Then the active loop cleanly detaches, keeping the process alive.
//
// Attach: `kbox attach [session-id]` calls POST /api/v1/shells/{id}/attach over a
// hijacked HTTP connection, then ServeRESTAttach replays scrollback and bridges the PTY.
//
// Sessions are garbage collected when detached with no client for over 30 minutes

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/creack/pty"
	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/utils"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// defaultDetachedSessionTTL is the fallback TTL for detached sessions when
	// no value is set in the server config or per-session override.
	defaultDetachedSessionTTL = 30 * time.Minute

	// detachableGCInterval controls how often the GC scans for expired sessions.
	detachableGCInterval = 1 * time.Minute
)

// streamSender abstracts sending terminal output bytes to a client
// The session-owned PTY read loop uses this so it can forward data without
// holding a concrete reference to the gRPC stream or REST connection.
type streamSender interface {
	send(data []byte) error
}

// grpcStreamSender implements streamSender for a bidi gRPC shell stream.
type grpcStreamSender struct {
	stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse]
}

// send forwards data to the client over the gRPC stream.  The stream's Recv loop
// handles any send errors by detaching the client
func (s *grpcStreamSender) send(data []byte) error {
	return s.stream.Send(&k8shelldv1.ShellResponse{
		Response: &k8shelldv1.ShellResponse_Data{Data: data},
	})
}

// connSender implements streamSender for a raw net.Conn (REST hijack attach).
type connSender struct{ conn net.Conn }

func (s *connSender) send(data []byte) error {
	_, err := s.conn.Write(data)
	return err
}

// doAttach registers sender as the current client and returns a fresh
// per-attachment detachRequested channel.  Must be called without holding mu.
func (session *SessionData) doAttach(sender streamSender) <-chan struct{} {
	ch := make(chan struct{})
	session.mu.Lock()
	session.attachedSender = sender
	session.DetachedAt = time.Time{}
	session.detachRequested = ch
	session.mu.Unlock()
	return ch
}

// startPtyReadLoop launches a goroutine that pumps PTY output into the ring
// buffer for the entire lifetime of the shell process, independent of any
// attached client.  While a client is attached the data is forwarded live.
// The goroutine closes session.ptyDone when the PTY is closed (shell exited).
func (s *ShellHandler) startPtyReadLoop(session *SessionData) {
	go func() {
		defer close(session.ptyDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := session.Ptmx.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				session.ring.Write(data)

				session.mu.Lock()
				w := session.attachedSender
				session.BytesOut += uint64(n)
				session.mu.Unlock()

				if w != nil {
					if sendErr := w.send(data); sendErr != nil {
						session.mu.Lock()
						if session.attachedSender == w {
							session.attachedSender = nil
						}
						session.mu.Unlock()
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// runAttachedClientLoop forwards gRPC client input to the PTY.
// Ctrl+A D triggers a detach (handled by filterPtyInput).  The loop also listens
// for detachCh (explicit detach), session.ptyDone (shell exit), and stream errors.
// When autoDetach is true, a gRPC context close (client disconnect) detaches the
// session rather than destroying it, keeping the shell alive.
func (s *ShellHandler) runAttachedClientLoop(
	logger *zerolog.Logger,
	session *SessionData,
	stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse],
	detachCh <-chan struct{},
	autoDetach bool,
) error {
	ctx := stream.Context()
	reqCh := make(chan *k8shelldv1.ShellRequest, 8)
	recvErrCh := make(chan error, 1)

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

	clearAttached := func() {
		session.mu.Lock()
		session.attachedSender = nil
		session.mu.Unlock()
	}
	doDetach := func() {
		session.mu.Lock()
		session.attachedSender = nil
		session.DetachedAt = time.Now()
		session.mu.Unlock()
		logger.Info().Msgf("Session %s: detached, process kept alive", session.Id)
	}

	prevCtrlA := false // cctrlA key press carried across successive Recv calls

	for {
		select {
		case <-ctx.Done():
			if autoDetach {
				doDetach()
				logger.Debug().Msgf("Session %s: gRPC context done, auto-detaching", session.Id)
			} else {
				clearAttached()
				logger.Debug().Msgf("Session %s: gRPC context done, destroying", session.Id)
			}
			return nil

		case <-session.ptyDone:
			clearAttached()
			s.grpcApi.SessionStore.Delete(session.Id)
			session.Deleted = time.Now()
			logger.Debug().Msgf("Session %s: shell exited", session.Id)
			return nil

		case <-detachCh:
			doDetach()
			return nil

		case err := <-recvErrCh:
			clearAttached()
			if err == io.EOF {
				logger.Debug().Msgf("Session %s: gRPC EOF, destroying", session.Id)
				return nil
			}
			return fmt.Errorf("stream error: %w", err)

		case req, ok := <-reqCh:
			if !ok {
				clearAttached()
				return nil
			}
			if data := req.GetData(); data != nil {
				filtered, detach := filterPtyInput(data, &prevCtrlA)
				if len(filtered) > 0 {
					if _, werr := session.Ptmx.Write(filtered); werr != nil {
						clearAttached()
						return fmt.Errorf("pty write: %w", werr)
					}
					session.mu.Lock()
					session.BytesIn += uint64(len(filtered))
					session.mu.Unlock()
				}
				if detach {
					if !s.grpcApi.allowSessionDetach {
						// pass Ctrl+A D through to the PTY unchanged
						_, _ = session.Ptmx.Write([]byte{0x01, 'd'})
					} else {
						doDetach()
						return nil
					}
				}
			}
			// Non-data messages (resize etc. are handled via dedicated RPCs) are ignored.
		}
	}
}

// **** REST attach/detach API (called from internal/server/restapi.go)

// DetachedSessionInfo is returned by ListDetachedSessions.
type DetachedSessionInfo struct {
	Id         string `json:"id"`
	CmdShell   string `json:"cmd_shell"`
	Pid        int    `json:"pid"`
	Created    string `json:"created"`
	DetachedAt string `json:"detached_at"`
}

// ListDetachedSessions returns sessions that are alive but have no attached client
func (a *GRPCService) ListDetachedSessions() []DetachedSessionInfo {
	var result []DetachedSessionInfo
	a.SessionStore.Range(func(key, value any) bool {
		session, ok := value.(*SessionData)
		if !ok || session.ptyDone == nil || !session.Deleted.IsZero() {
			return true
		}
		select {
		case <-session.ptyDone:
			return true
		default:
		}
		session.mu.Lock()
		detachedAt := session.DetachedAt
		attached := session.attachedSender != nil
		session.mu.Unlock()

		if attached || detachedAt.IsZero() {
			return true
		}
		result = append(result, DetachedSessionInfo{
			Id:         session.Id,
			CmdShell:   session.CmdShell,
			Pid:        session.Pid,
			Created:    session.Created.Format(time.RFC3339),
			DetachedAt: detachedAt.Format(time.RFC3339),
		})
		return true
	})
	return result
}

// handleGRPCAttachExisting reattaches a gRPC stream to an already-detached session.
// It replays the scrollback ring buffer, registers the stream as the sender, then
// runs the normal attached-client loop with autoDetach=true so that a client
// disconnect keeps the session alive rather than destroying it.
func (s *ShellHandler) handleGRPCAttachExisting(
	stream grpc.BidiStreamingServer[k8shelldv1.ShellRequest, k8shelldv1.ShellResponse],
	session *SessionData,
) error {
	_ = stream.Send(&k8shelldv1.ShellResponse{
		Response: &k8shelldv1.ShellResponse_StartResponse{
			StartResponse: &k8shelldv1.ShellStartResponse{},
		},
	})

	if scrollback := session.ring.Snapshot(); len(scrollback) > 0 {
		_ = stream.Send(&k8shelldv1.ShellResponse{
			Response: &k8shelldv1.ShellResponse_Data{Data: scrollback},
		})
	}

	detachCh := session.doAttach(&grpcStreamSender{stream: stream})
	return s.runAttachedClientLoop(s.logger, session, stream, detachCh, true)
}

// httpStatusToGRPCCode maps HTTP status codes returned by ValidateSessionForAttach
// to the nearest equivalent gRPC status code.
func httpStatusToGRPCCode(httpCode int) codes.Code {
	switch httpCode {
	case 403:
		return codes.PermissionDenied
	case 404:
		return codes.NotFound
	case 409:
		return codes.AlreadyExists
	case 410:
		return codes.NotFound
	default:
		return codes.InvalidArgument
	}
}

// DetachShellSession triggers a detach on the currently attached client for
// the given session, optionally setting a per-session TTL override.
// The shell process and ring buffer stay alive.
func (a *GRPCService) DetachShellSession(sessionId string, ttl *time.Duration) error {
	if !a.allowSessionDetach {
		return fmt.Errorf("session detachment is not enabled")
	}
	v, ok := a.SessionStore.Load(sessionId)
	if !ok {
		return fmt.Errorf("session %s not found", sessionId)
	}
	session := v.(*SessionData)

	if ttl != nil {
		if err := a.validateTTL(*ttl); err != nil {
			return err
		}
		ttlCopy := *ttl
		session.mu.Lock()
		session.DetachTTL = &ttlCopy
		session.mu.Unlock()
	}

	session.mu.Lock()
	ch := session.detachRequested
	session.mu.Unlock()

	if ch == nil {
		return fmt.Errorf("session %s has no active attachment", sessionId)
	}
	select {
	case <-ch: // already fired — no-op
	default:
		close(ch)
	}
	return nil
}

// ValidateSessionForAttach checks that a session exists, is a live PTY session,
// and has no client currently attached.
func (a *GRPCService) ValidateSessionForAttach(sessionId string) (int, error) {
	if !a.allowSessionDetach {
		return 403, fmt.Errorf("session attachment is not enabled")
	}
	v, ok := a.SessionStore.Load(sessionId)
	if !ok {
		return 404, fmt.Errorf("session %s not found", sessionId)
	}
	session := v.(*SessionData)
	if session.ptyDone == nil {
		return 400, fmt.Errorf("session %s is not a PTY session", sessionId)
	}
	select {
	case <-session.ptyDone:
		return 410, fmt.Errorf("session %s has already exited", sessionId)
	default:
	}
	session.mu.Lock()
	attached := session.attachedSender != nil
	session.mu.Unlock()
	if attached {
		return 409, fmt.Errorf("session %s already has a client attached", sessionId)
	}
	return 0, nil
}

// validateTTL returns an error if the requested TTL violates server policy.
// ttl=0 means "never expire" and requires allowUnlimitedTTL.
// Non-zero values are capped at detachedSessionTTL (0 = no cap).
func (a *GRPCService) validateTTL(requested time.Duration) error {
	if requested == 0 {
		if !a.allowUnlimitedTTL {
			if a.detachedSessionTTL == 0 {
				return fmt.Errorf("ttl=0 (no expiry) is not permitted; set allowUnlimittedTTL in server config to enable it")
			}
			return fmt.Errorf("ttl=0 (no expiry) is not permitted; server maximum is %v", a.detachedSessionTTL)
		}
		return nil
	}
	max := a.detachedSessionTTL
	if max != 0 && requested > max {
		return fmt.Errorf("requested ttl %v exceeds server maximum of %v", requested, max)
	}
	return nil
}

// ServeRESTAttach attaches a hijacked HTTP conn to the session: replays the
// ring-buffer scrollback, registers the conn as the current sender, then bridges
// PTY I/O until the session ends or the client calls `kbox detach`.
// Blocks until the session is destroyed or detached.
func (a *GRPCService) ServeRESTAttach(sessionId string, conn net.Conn) error {
	v, ok := a.SessionStore.Load(sessionId)
	if !ok {
		return fmt.Errorf("session %s not found", sessionId)
	}
	session := v.(*SessionData)

	scrollback := session.ring.Snapshot()
	detachCh := session.doAttach(&connSender{conn: conn})

	if len(scrollback) > 0 {
		if _, err := conn.Write(scrollback); err != nil {
			session.mu.Lock()
			session.attachedSender = nil
			session.DetachedAt = time.Now()
			session.mu.Unlock()
			return fmt.Errorf("write scrollback: %w", err)
		}
	}

	a.logger.Info().Msgf("Attached to shell session %s (pid %d)", sessionId, session.Pid)
	a.runRESTAttachLoop(session, conn, detachCh)
	return nil
}

// runRESTAttachLoop bridges a raw net.Conn to the PTY.
// It listens for client input, session exit, and detach signals. When the client sends Ctrl+A D
// or calls `kbox detach`, the session is detached but remains alive.  When the session exits
// or the client disconnects without detaching, the session is destroyed and removed from the store
func (a *GRPCService) runRESTAttachLoop(session *SessionData, conn net.Conn, detachCh <-chan struct{}) {
	defer conn.Close()

	inputErrCh := make(chan error, 1)
	clientDetachCh := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 4096)
		prevCtrlA := false
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				filtered, detach := filterPtyInput(buf[:n], &prevCtrlA)
				if len(filtered) > 0 {
					session.mu.Lock()
					ptmx := session.Ptmx
					session.BytesIn += uint64(len(filtered))
					session.mu.Unlock()
					if ptmx != nil {
						if _, werr := ptmx.Write(filtered); werr != nil {
							inputErrCh <- werr
							return
						}
					}
				}
				if detach {
					if !a.allowSessionDetach {
						// pass Ctrl+A D through to the PTY unchanged
						session.mu.Lock()
						ptmx := session.Ptmx
						session.mu.Unlock()
						if ptmx != nil {
							_, _ = ptmx.Write([]byte{0x01, 'd'})
						}
					} else {
						clientDetachCh <- struct{}{}
						return
					}
				}
			}
			if err != nil {
				inputErrCh <- err
				return
			}
		}
	}()

	doDetach := func() {
		session.mu.Lock()
		session.attachedSender = nil
		session.DetachedAt = time.Now()
		session.mu.Unlock()
		a.logger.Info().Msgf("REST: session %s detached, process kept alive", session.Id)
	}
	doDestroy := func(reason string) {
		session.mu.Lock()
		session.attachedSender = nil
		session.mu.Unlock()
		session.cleanup()
		session.Deleted = time.Now()
		a.SessionStore.Delete(session.Id)
		a.logger.Info().Msgf("REST: session %s destroyed: %s", session.Id, reason)
	}

	select {
	case <-session.ptyDone:
		doDestroy("shell exited")
	case <-detachCh:
		doDetach()
	case <-clientDetachCh:
		doDetach()
	case err := <-inputErrCh:
		a.logger.Debug().Msgf("REST: session %s conn closed: %v", session.Id, err)
		doDestroy("client disconnected without detach")
	}
}

// filterPtyInput scans data written to a PTY for the Ctrl+A D detach sequence (0x01 0x64)
func filterPtyInput(data []byte, prevCtrlA *bool) (out []byte, detach bool) {
	out = make([]byte, 0, len(data))
	for _, b := range data {
		if *prevCtrlA {
			*prevCtrlA = false
			if b == 'd' || b == 'D' {
				return out, true
			}
			out = append(out, 0x01, b)
			continue
		}
		if b == 0x01 { // Ctrl+A: hold until next byte
			*prevCtrlA = true
			continue
		}
		out = append(out, b)
	}
	return out, false
}

// ResizeSession resizes the PTY of any shell session (attached or detached).
func (a *GRPCService) ResizeSession(sessionId string, width, height uint32) error {
	v, ok := a.SessionStore.Load(sessionId)
	if !ok {
		return status.Errorf(codes.NotFound, "session %s not found", sessionId)
	}
	session := v.(*SessionData)
	if session.Ptmx == nil {
		return status.Errorf(codes.FailedPrecondition, "session %s has no PTY", sessionId)
	}
	return pty.Setsize(session.Ptmx, &pty.Winsize{
		Rows: utils.ClampUint32ToUint16(height),
		Cols: utils.ClampUint32ToUint16(width),
	})
}

// gcDetachedSessions removes expired sessions.
func (a *GRPCService) gcDetachedSessions() {
	now := time.Now()
	a.SessionStore.Range(func(key, value any) bool {
		session, ok := value.(*SessionData)
		if !ok || session.ptyDone == nil {
			return true
		}
		select {
		case <-session.ptyDone:
			if session.Deleted.IsZero() {
				a.logger.Info().Msgf("GC: marking exited session %s as stopped", session.Id)
				session.cleanup()
				session.Deleted = now
			}
			return true
		default:
		}
		session.mu.Lock()
		isDetached := session.attachedSender == nil
		detachedAt := session.DetachedAt
		session.mu.Unlock()

		if isDetached && !detachedAt.IsZero() {
			var effectiveTTL time.Duration
			session.mu.Lock()
			perSession := session.DetachTTL
			session.mu.Unlock()
			if perSession != nil {
				effectiveTTL = *perSession
			} else {
				effectiveTTL = a.detachedSessionTTL
			}
			if effectiveTTL == 0 {
				return true // this session is exempt from GC
			}
			if now.Sub(detachedAt) > effectiveTTL {
				a.logger.Info().Msgf("GC: session %s timed out after %v (ttl=%v), terminating",
					session.Id, now.Sub(detachedAt).Round(time.Second), effectiveTTL)
				session.cleanup()
				session.Deleted = now
			}
		}
		return true
	})
}
