package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/k8shell-io/k8shelld/internal/client"
	grpcpkg "github.com/k8shell-io/k8shelld/internal/grpc"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

var AttachCmd = &cobra.Command{
	Use:   "attach [session-id]",
	Short: "Attach to a detached shell session",
	Long: `Attach to a detached shell session by session ID.

If no session ID is provided, lists all detached sessions and prompts you
to select one. The terminal is put into raw mode for the duration of the
session.

Detach shortcut: Ctrl+A D  (same as GNU screen)
  Keeps the shell process running; use 'kbox attach' to reconnect.`,

	Run: func(cmd *cobra.Command, args []string) {
		var id string

		if len(args) > 0 {
			id = args[0]
		} else {
			var err error
			id, err = pickSession()
			if err != nil {
				fmt.Fprintf(os.Stderr, "attach: %v\n", err)
				os.Exit(1)
			}
		}

		conn, err := client.HijackAttach(id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "attach: %v\n", err)
			os.Exit(1)
		}
		defer conn.Close()

		// Put stdin in raw mode so all key events are forwarded as-is.
		// The server intercepts Ctrl+A D and closes the connection cleanly.
		fd := int(os.Stdin.Fd())
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "attach: raw mode: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = term.Restore(fd, oldState) }()

		// Send the initial terminal size and forward any subsequent resize events.
		sendResize := func() {
			w, h, err := term.GetSize(fd)
			if err != nil || w <= 0 || h <= 0 {
				return
			}
			body, _ := json.Marshal(struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			}{w, h})
			_, _ = client.MakeRequest("POST", "/shells/"+id+"/resize", nil, bytes.NewReader(body))
		}
		sendResize()

		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for range winch {
				sendResize()
			}
		}()
		defer func() {
			signal.Stop(winch)
			close(winch)
		}()

		// conn -> stdout: runs until the server closes the connection (detach or
		// session exit). Signals outDone so the main goroutine can clean up.
		outDone := make(chan struct{})
		go func() {
			defer close(outDone)
			_, _ = io.Copy(os.Stdout, conn)
		}()

		// stdin -> conn (unfiltered; detach interception is server-side).
		// If stdin closes first, shut down the conn so the output goroutine exits.
		go func() {
			_, _ = io.Copy(conn, os.Stdin)
			conn.Close()
		}()

		// Wait for the output side to finish (server closed the connection).
		<-outDone

		// Flush any terminal query responses (e.g. CPR, DA2, OSC color replies)
		// that the terminal emulator queued in stdin while we were in raw mode.
		// This must happen BEFORE term.Restore re-enables ECHO; otherwise the
		// line discipline echoes those bytes onto the parent shell's prompt.
		_ = unix.IoctlSetInt(fd, unix.TCFLSH, 0 /* TCIFLUSH */)
	},
}

// pickSession fetches the list of detached sessions and prompts the user to
// select one, returning its ID.
func pickSession() (string, error) {
	resp, err := client.MakeRequest("GET", "/shells", nil, nil)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	defer resp.Body.Close()

	if err := client.CheckApplicationError(resp); err != nil {
		return "", err
	}

	var sessions []grpcpkg.DetachedSessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(sessions) == 0 {
		return "", fmt.Errorf("no detached sessions available")
	}

	if len(sessions) == 1 {
		fmt.Printf("Attaching to the only detached session: %s\n", sessions[0].Id)
		return sessions[0].Id, nil
	}

	// Print a table and ask the user to choose.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tID\tSHELL\tPID\tCREATED\tDETACHED")
	for i, s := range sessions {
		created := formatRelTime(s.Created)
		detached := formatRelTime(s.DetachedAt)
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\n",
			i+1, s.Id, s.CmdShell, s.Pid, created, detached)
	}
	tw.Flush()

	fmt.Printf("\nEnter number (1-%d): ", len(sessions))
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return "", fmt.Errorf("no input")
	}
	choice := strings.TrimSpace(scanner.Text())
	n := 0
	if _, err := fmt.Sscanf(choice, "%d", &n); err != nil || n < 1 || n > len(sessions) {
		return "", fmt.Errorf("invalid selection %q", choice)
	}
	return sessions[n-1].Id, nil
}

// formatRelTime parses an RFC3339 string and returns a human-friendly relative
// duration, e.g. "5m ago".
func formatRelTime(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String() + " ago"
}
