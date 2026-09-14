// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// acquire.go — AcquireSession RPC and session lock management.
//
// The acquire/attach flow:
//  1. Client calls AcquireSession(session_id).
//     Server checks the session exists, is a live PTY, has no current client attached,
//     and has no unexpired lock. On success it creates a time-limited lock and returns
//     a lock_id.
//  2. Client calls Shell(StartRequest{lock_id: ..., use_pty: true, ...}).
//     Server validates the lock_id, consumes it, then attaches to the existing session.

package grpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sessionLock represents a short-lived claim on a detached session.
type sessionLock struct {
	sessionId string
	expiresAt time.Time
}

// generateLockID returns a unique lock ID with a "lk-" prefix.
func generateLockID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate lock ID: %w", err)
	}
	return "lk-" + hex.EncodeToString(b), nil
}

// AcquireSession implements SshServiceServer.AcquireSession.
// It validates that the requested session exists, is a live detached PTY session
// with no client attached and no unexpired lock, then creates and returns a lock ID
func (s *ShellHandler) AcquireSession(ctx context.Context, req *k8shelldv1.AcquireSessionRequest) (*k8shelldv1.AcquireSessionResponse, error) {
	if !s.grpcApi.allowSessionDetach {
		return nil, status.Errorf(codes.PermissionDenied, "session attachment is not enabled on this server")
	}

	sessionId := req.GetSessionId()
	if sessionId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "session_id must not be empty")
	}

	code, err := s.grpcApi.ValidateSessionForAttach(sessionId)
	if err != nil {
		reason := k8shelldv1.AcquireSessionFailureReason_ACQUIRE_SESSION_FAILURE_REASON_UNSPECIFIED
		switch code {
		case 404, 410:
			reason = k8shelldv1.AcquireSessionFailureReason_ACQUIRE_SESSION_FAILURE_REASON_NOT_FOUND
		case 403, 409:
			reason = k8shelldv1.AcquireSessionFailureReason_ACQUIRE_SESSION_FAILURE_REASON_ALREADY_ACQUIRED
		}
		return &k8shelldv1.AcquireSessionResponse{
			Success:       false,
			FailureReason: reason,
		}, nil
	}

	lockId, err := generateLockID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to generate lock ID: %v", err)
	}

	s.grpcApi.acquireMu.Lock()
	now := time.Now()
	var existingLock bool
	s.grpcApi.SessionLockStore.Range(func(_, v any) bool {
		lk := v.(*sessionLock)
		if lk.sessionId == sessionId && now.Before(lk.expiresAt) {
			existingLock = true
			return false
		}
		return true
	})
	if existingLock {
		s.grpcApi.acquireMu.Unlock()
		return &k8shelldv1.AcquireSessionResponse{
			Success:       false,
			FailureReason: k8shelldv1.AcquireSessionFailureReason_ACQUIRE_SESSION_FAILURE_REASON_ALREADY_ACQUIRED,
		}, nil
	}
	s.grpcApi.SessionLockStore.Store(lockId, &sessionLock{
		sessionId: sessionId,
		expiresAt: now.Add(sessionLockTTL),
	})
	s.grpcApi.acquireMu.Unlock()

	s.grpcApi.logger.Info().Msgf("AcquireSession: lock %s created for session %s (expires in %v)", lockId, sessionId, sessionLockTTL)

	return &k8shelldv1.AcquireSessionResponse{
		Success: true,
		LockId:  lockId,
	}, nil
}

// ListSessions implements SshServiceServer.ListSessions.
// It returns the set of live PTY sessions that AcquireSession would currently
// accept: no client attached and no unexpired lock held, along with the OS
// user each session runs as.
func (s *ShellHandler) ListSessions(_ context.Context, _ *k8shelldv1.ListSessionsRequest) (*k8shelldv1.ListSessionsResponse, error) {
	if !s.grpcApi.allowSessionDetach {
		return nil, status.Errorf(codes.PermissionDenied, "session attachment is not enabled on this server")
	}

	now := time.Now()
	locked := make(map[string]bool)
	s.grpcApi.SessionLockStore.Range(func(_, v any) bool {
		lk := v.(*sessionLock)
		if now.Before(lk.expiresAt) {
			locked[lk.sessionId] = true
		}
		return true
	})

	resp := &k8shelldv1.ListSessionsResponse{}
	s.grpcApi.SessionStore.Range(func(_, value any) bool {
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
		attached := session.attachedSender != nil
		detachedAt := session.DetachedAt
		session.mu.Unlock()

		if attached || locked[session.Id] {
			return true
		}

		var detachedAtStr string
		if !detachedAt.IsZero() {
			detachedAtStr = detachedAt.Format(timeFormat)
		}

		resp.Sessions = append(resp.Sessions, &k8shelldv1.AcquirableSession{
			SessionId:  session.Id,
			Owner:      session.user.Username,
			CmdShell:   session.CmdShell,
			Pid:        int32(session.Pid),
			Created:    session.Created.Format(timeFormat),
			DetachedAt: detachedAtStr,
		})
		return true
	})

	return resp, nil
}

// cleanupExpiredLocks removes session locks that have passed their TTL.
func (a *GRPCService) cleanupExpiredLocks() {
	now := time.Now()
	a.SessionLockStore.Range(func(k, v any) bool {
		if now.After(v.(*sessionLock).expiresAt) {
			a.SessionLockStore.Delete(k)
			a.logger.Debug().Msgf("Session lock %s expired and removed", k)
		}
		return true
	})
}
