// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package grpc

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	k8shelldv1 "github.com/k8shell-io/common/pkg/api/gen/go/k8shelld/v1"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func TestResolveShell(t *testing.T) {
	cases := []struct {
		name       string
		candidates []string
		want       string
	}{
		{"first usable wins", []string{"/bin/sh", "/does/not/exist"}, "/bin/sh"},
		{"missing hint falls through", []string{"/does/not/exist", "/bin/sh"}, "/bin/sh"},
		{"empty hint falls through", []string{"", "/bin/sh"}, "/bin/sh"},
		{"nologin is skipped", []string{"/usr/sbin/nologin", "/sbin/nologin"}, fallbackShell},
		{"directory is not a shell", []string{"/tmp"}, fallbackShell},
		{"nothing resolves", nil, fallbackShell},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveShell(c.candidates...); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestUserEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/root")
	user := models.ShellUser{Username: "alice", HomeDir: "/home/alice"}

	env := envMap(userEnv([]string{"TERM=xterm-256color", "SHELL=/bin/zsh", "HOME=/nope"}, user, "/bin/bash"))

	want := map[string]string{
		"TERM":    "xterm-256color", // client supplied
		"SHELL":   "/bin/zsh",       // client wins over the resolved shell
		"USER":    "alice",          // default when not supplied
		"LOGNAME": "alice",
		"HOME":    "/home/alice", // always the user's home
		"PATH":    "/usr/bin:/bin",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if _, ok := m[k]; ok {
			m[k] = "<duplicate>"
			continue
		}
		m[k] = v
	}
	return m
}

// fakeExecStream is an in-memory ExecRequest/ExecResponse bidi stream.
type fakeExecStream struct {
	gogrpc.ServerStream
	ctx  context.Context
	reqs chan *k8shelldv1.ExecRequest
	mu   sync.Mutex
	resp []*k8shelldv1.ExecResponse
}

func newFakeExecStream(execID string, first *k8shelldv1.CommandDetails) *fakeExecStream {
	f := &fakeExecStream{
		ctx:  metadata.NewIncomingContext(context.Background(), metadata.Pairs("exec-id", execID)),
		reqs: make(chan *k8shelldv1.ExecRequest, 16),
	}
	f.reqs <- &k8shelldv1.ExecRequest{Request: &k8shelldv1.ExecRequest_CommandDetails{CommandDetails: first}}
	return f
}

func (f *fakeExecStream) Context() context.Context { return f.ctx }

func (f *fakeExecStream) Recv() (*k8shelldv1.ExecRequest, error) {
	r, ok := <-f.reqs
	if !ok {
		return nil, io.EOF
	}
	return r, nil
}

func (f *fakeExecStream) Send(r *k8shelldv1.ExecResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Exec reuses its read buffer after Send returns, as gRPC allows.
	f.resp = append(f.resp, proto.Clone(r).(*k8shelldv1.ExecResponse))
	return nil
}

// result collects the responses once Exec has returned.
func (f *fakeExecStream) result() (pty, stdout, stderr string, exitCode int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	exitCode = -1
	for _, r := range f.resp {
		switch r.Response.(type) {
		case *k8shelldv1.ExecResponse_Pty:
			pty = r.GetPty()
		case *k8shelldv1.ExecResponse_Stdout:
			stdout += string(r.GetStdout())
		case *k8shelldv1.ExecResponse_Stderr:
			stderr += string(r.GetStderr())
		case *k8shelldv1.ExecResponse_ExitCode:
			exitCode = r.GetExitCode()
		}
	}
	return
}

func runExec(t *testing.T, details *k8shelldv1.CommandDetails, feed func(f *fakeExecStream)) *fakeExecStream {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("exec tests need root to set process credentials")
	}
	h := newExecHandler(&GRPCService{
		procWatcher: system.NewProcessWatcher(false, false, 0, nil),
		ExecStore:   &sync.Map{},
	})
	details.AsUser = "root"
	f := newFakeExecStream(t.Name(), details)
	if feed != nil {
		go feed(f)
	}
	done := make(chan error, 1)
	go func() { done <- h.Exec(f) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec did not return")
	}
	return f
}

func TestExecNoPty(t *testing.T) {
	f := runExec(t, &k8shelldv1.CommandDetails{
		Command:     `echo out; echo err >&2; [ -t 0 ] && echo tty; exit 3`,
		ShellBinary: "/bin/sh",
	}, nil)
	pty, stdout, stderr, code := f.result()
	if pty != "" || stdout != "out\n" || stderr != "err\n" || code != 3 {
		t.Errorf("pty=%q stdout=%q stderr=%q code=%d", pty, stdout, stderr, code)
	}
}

func TestExecLoginShellFallback(t *testing.T) {
	want := resolveShell(loginShell("root"))
	f := runExec(t, &k8shelldv1.CommandDetails{
		Command:     `echo "$SHELL"`,
		ShellBinary: "/does/not/exist",
	}, nil)
	_, stdout, _, code := f.result()
	if strings.TrimSpace(stdout) != want || code != 0 {
		t.Errorf("stdout=%q code=%d, want shell %q", stdout, code, want)
	}
}

func TestExecPty(t *testing.T) {
	f := runExec(t, &k8shelldv1.CommandDetails{
		Command:     `stty size; read l; stty size; echo "got $l" >&2; tty`,
		ShellBinary: "/bin/sh",
		EnvVars:     []string{"TERM=xterm-256color"},
		UsePty:      true,
		Width:       100,
		Height:      30,
	}, func(f *fakeExecStream) {
		time.Sleep(200 * time.Millisecond)
		f.reqs <- &k8shelldv1.ExecRequest{Request: &k8shelldv1.ExecRequest_Resize{
			Resize: &k8shelldv1.TerminalSize{Width: 120, Height: 40},
		}}
		time.Sleep(200 * time.Millisecond)
		f.reqs <- &k8shelldv1.ExecRequest{Request: &k8shelldv1.ExecRequest_Input{Input: []byte("hi\n")}}
		close(f.reqs)
	})
	pty, stdout, stderr, code := f.result()
	if !strings.HasPrefix(pty, "/dev/pts/") {
		t.Errorf("pty = %q", pty)
	}
	if stderr != "" {
		t.Errorf("stderr should be merged into stdout with a PTY, got %q", stderr)
	}
	for _, want := range []string{"30 100\r\n", "40 120\r\n", "got hi\r\n", pty + "\r\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout %q does not contain %q", stdout, want)
		}
	}
	if code != 0 {
		t.Errorf("exit code = %d", code)
	}
}
