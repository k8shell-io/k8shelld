package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/k8shell-io/k8shelld/pkg/common"
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

// all methods to make request to the API server; kbox does not call api server directly
// instead, it uses the k8shelld unix socket to communicate with the API server

// // getToken retrieves an access token from the k8shelld via unix socket
// func getToken() (string, error) {
// 	url := "/token"
// 	resp, err := MakeRequest("GET", url, nil, nil)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to get token: %v", err)
// 	}
// 	defer resp.Body.Close()

// 	var tokenResponse struct {
// 		Token string `json:"token"`
// 	}
// 	bodyBytes, _ := io.ReadAll(resp.Body)
// 	if err := json.Unmarshal(bodyBytes, &tokenResponse); err != nil {
// 		return "", fmt.Errorf("failed to unmarshal token response: %v", err)
// 	}

// 	return tokenResponse.Token, nil
// }

// // MakeRequest makes an HTTP request to the API server
// func MakeRequest(method string, url string, headers map[string]string) (string, error) {
// 	token, err := getToken()
// 	if err != nil {
// 		return "", fmt.Errorf("failed to get token: %v", err)
// 	}

// 	baseURL := "http://api-internal/api/v1"
// 	workspace := os.Getenv("WORKSPACE")
// 	fullURL := fmt.Sprintf("%s/workspaces/%s/%s", baseURL, workspace, url)

// 	req, err := http.NewRequest(method, fullURL, nil)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to create request: %v", err)
// 	}

// 	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
// 	for key, value := range headers {
// 		req.Header.Set(key, value)
// 	}

// 	client := &http.Client{Timeout: 1000 * time.Millisecond}

// 	sanitizedHeaders := make(map[string]string)
// 	for key, values := range req.Header {
// 		if strings.ToLower(key) == "authorization" {
// 			sanitizedHeaders[key] = "***"
// 		} else {
// 			sanitizedHeaders[key] = values[0]
// 		}
// 	}

// 	resp, err := client.Do(req)
// 	if err != nil {
// 		return "", fmt.Errorf("request failed: %v", err)
// 	}
// 	defer resp.Body.Close()

// 	body, err := io.ReadAll(resp.Body)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to read response body: %v", err)
// 	}

// 	if resp.StatusCode != http.StatusOK {
// 		var errorResponse struct {
// 			Errno   int    `json:"errno"`
// 			Message string `json:"message"`
// 		}

// 		if err := json.Unmarshal(body, &errorResponse); err != nil {
// 			return string(body), fmt.Errorf("API call failed with status %d: %s", resp.StatusCode, body)
// 		}

// 		return string(body), fmt.Errorf("%s", errorResponse.Message)
// 	}

// 	return string(body), nil
// }
