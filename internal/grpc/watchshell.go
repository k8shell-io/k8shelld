package grpc

import (
	"fmt"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"google.golang.org/grpc"
)

const cwdPollInterval = 500 * time.Millisecond

// WatchShell streams CWD and filesystem notifications for a running shell session.
// The client subscribes to specific event types via the request; events are delivered
// until the stream context is cancelled or the shell process exits.
func (s *ShellServiceServer) WatchShell(req *k8shelldv1.WatchShellRequest, stream grpc.ServerStreamingServer[k8shelldv1.WatchShellEvent]) error {
	ctx := stream.Context()

	session, err := s.GetSessionData(ctx)
	if err != nil {
		return err
	}

	watchCWD := false
	watchFS := false
	for _, et := range req.GetEventTypes() {
		switch et {
		case k8shelldv1.WatchShellEventType_WATCH_SHELL_EVENT_TYPE_CWD:
			watchCWD = true
		case k8shelldv1.WatchShellEventType_WATCH_SHELL_EVENT_TYPE_FS:
			watchFS = true
		}
	}

	if !watchCWD && !watchFS {
		<-ctx.Done()
		return nil
	}

	eventCh := make(chan *k8shelldv1.WatchShellEvent, 32)
	// cwdCh delivers the new CWD path to the FS watcher whenever the shell changes directory.
	cwdCh := make(chan string, 4)

	procCWDPath := fmt.Sprintf("/proc/%d/cwd", session.Pid)

	// CWD polling goroutine: reads /proc/<pid>/cwd every cwdPollInterval and emits a
	// CwdChangedEvent when the value changes. Also feeds cwdCh so the FS watcher can
	// retarget itself after a directory change.
	go func() {
		ticker := time.NewTicker(cwdPollInterval)
		defer ticker.Stop()

		var lastCWD string

		sendCWD := func(cwd string) {
			if watchCWD {
				select {
				case eventCh <- &k8shelldv1.WatchShellEvent{
					Event: &k8shelldv1.WatchShellEvent_CwdChanged{
						CwdChanged: &k8shelldv1.CwdChangedEvent{Path: cwd},
					},
				}:
				default:
				}
			}
			if watchFS {
				select {
				case cwdCh <- cwd:
				default:
				}
			}
		}

		// Seed the initial CWD so the FS watcher starts watching immediately.
		if cwd, readErr := os.Readlink(procCWDPath); readErr == nil {
			lastCWD = cwd
			sendCWD(cwd)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cwd, readErr := os.Readlink(procCWDPath)
				if readErr != nil {
					// Process likely exited; stop polling.
					return
				}
				if cwd != lastCWD {
					lastCWD = cwd
					sendCWD(cwd)
				}
			}
		}
	}()

	// FS watcher goroutine: uses inotify (via fsnotify) to deliver file-system events for the
	// shell's current working directory. Re-targets the watch whenever the CWD changes.
	if watchFS {
		go func() {
			watcher, watcherErr := fsnotify.NewWatcher()
			if watcherErr != nil {
				s.logger.Error().Err(watcherErr).Msg("WatchShell: failed to create fsnotify watcher")
				return
			}
			defer watcher.Close()

			var currentWatch string

			mapFsEventType := func(op fsnotify.Op) k8shelldv1.FsEventType {
				switch {
				case op.Has(fsnotify.Create):
					return k8shelldv1.FsEventType_FS_EVENT_TYPE_CREATE
				case op.Has(fsnotify.Remove):
					return k8shelldv1.FsEventType_FS_EVENT_TYPE_DELETE
				case op.Has(fsnotify.Rename):
					return k8shelldv1.FsEventType_FS_EVENT_TYPE_RENAME
				case op.Has(fsnotify.Write):
					return k8shelldv1.FsEventType_FS_EVENT_TYPE_MODIFY
				default:
					return k8shelldv1.FsEventType_FS_EVENT_TYPE_UNSPECIFIED
				}
			}

			for {
				select {
				case <-ctx.Done():
					return

				case newCWD, ok := <-cwdCh:
					if !ok {
						return
					}
					if newCWD == currentWatch {
						continue
					}
					if currentWatch != "" {
						_ = watcher.Remove(currentWatch)
					}
					if addErr := watcher.Add(newCWD); addErr != nil {
						s.logger.Warn().Err(addErr).Str("path", newCWD).Msg("WatchShell: failed to watch directory")
						currentWatch = ""
					} else {
						currentWatch = newCWD
					}

				case fsEv, ok := <-watcher.Events:
					if !ok {
						return
					}
					evType := mapFsEventType(fsEv.Op)
					if evType == k8shelldv1.FsEventType_FS_EVENT_TYPE_UNSPECIFIED {
						continue
					}
					select {
					case eventCh <- &k8shelldv1.WatchShellEvent{
						Event: &k8shelldv1.WatchShellEvent_FsEvent{
							FsEvent: &k8shelldv1.FsEvent{
								Path: fsEv.Name,
								Type: evType,
							},
						},
					}:
					default:
					}

				case watchErr, ok := <-watcher.Errors:
					if !ok {
						return
					}
					s.logger.Warn().Err(watchErr).Msg("WatchShell: fsnotify error")
				}
			}
		}()
	}

	// Main send loop: drains eventCh and forwards events to the client stream.
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-eventCh:
			if sendErr := stream.Send(ev); sendErr != nil {
				return sendErr
			}
		}
	}
}
