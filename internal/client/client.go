package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/k8shell-io/k8shelld/internal/common"
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
	client := newUnixSocketClient(common.DefaultRESTAPIUnixSocket)
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

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API call failed with status %d: %s", resp.StatusCode, bodyBytes)
	}
	return resp, nil
}
