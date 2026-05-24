package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/k8shell-io/k8shelld/internal/models"
)

// Custom HTTP client that communicates over a Unix socket
func newUnixSocketClient(socketPath string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}
	return &http.Client{Transport: transport}
}

func MakeRequest(method string, url string, headers map[string]string, data io.Reader) (*http.Response, error) {
	client := newUnixSocketClient(models.RESTAPIUnixSocket)
	req, err := http.NewRequest(method, "http://unix/api/v1"+url, data)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %v", err)
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make HTTP request: %v", err)
	}

	if resp.StatusCode >= 500 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(bodyBytes)))
	}
	return resp, nil
}

func CheckApplicationError(resp *http.Response) error {
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", bodyBytes)
	}
	return nil
}

// HijackAttach opens a raw connection to POST /api/v1/shells/{id}/attach.
// It sends the HTTP request manually, reads the "HTTP/1.1 101 Switching Protocols"
// response, drains the remaining headers, and returns the bare net.Conn ready for PTY I/O.
func HijackAttach(sessionId string) (net.Conn, error) {
	conn, err := net.Dial("unix", models.RESTAPIUnixSocket)
	if err != nil {
		return nil, fmt.Errorf("dial unix socket: %w", err)
	}

	req := fmt.Sprintf("POST /api/v1/shells/%s/attach HTTP/1.1\r\nHost: unix\r\nContent-Length: 0\r\n\r\n", sessionId)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write HTTP request: %w", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read HTTP response: %w", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 101") {
		conn.Close()
		return nil, fmt.Errorf("unexpected response: %s", strings.TrimSpace(statusLine))
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read HTTP headers: %w", err)
		}
		if line == "\r\n" {
			break
		}
	}

	return &bufferedConn{conn: conn, reader: reader}, nil
}

// bufferedConn wraps a net.Conn with a pre-filled bufio.Reader so that bytes
// already consumed from the socket into the buffer are not lost.
type bufferedConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error)         { return b.reader.Read(p) }
func (b *bufferedConn) Write(p []byte) (int, error)        { return b.conn.Write(p) }
func (b *bufferedConn) Close() error                       { return b.conn.Close() }
func (b *bufferedConn) LocalAddr() net.Addr                { return b.conn.LocalAddr() }
func (b *bufferedConn) RemoteAddr() net.Addr               { return b.conn.RemoteAddr() }
func (b *bufferedConn) SetDeadline(t time.Time) error      { return b.conn.SetDeadline(t) }
func (b *bufferedConn) SetReadDeadline(t time.Time) error  { return b.conn.SetReadDeadline(t) }
func (b *bufferedConn) SetWriteDeadline(t time.Time) error { return b.conn.SetWriteDeadline(t) }

// GetInitScripts fetches a snapshot of all init script states from the daemon.
func GetInitScripts() ([]models.InitScriptState, error) {
	resp, err := MakeRequest("GET", "/initscripts", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckApplicationError(resp); err != nil {
		return nil, err
	}
	var states []models.InitScriptState
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		return nil, fmt.Errorf("decode initscripts response: %w", err)
	}
	return states, nil
}
