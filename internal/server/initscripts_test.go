package server

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/k8shell-io/k8shelld/grpc/generated-go/k8shelldpb"
	"github.com/k8shell-io/k8shelld/internal/log"
	"google.golang.org/grpc/metadata"
)

// mockStreamServer implements grpc.ServerStreamingServer[k8shelldpb.InitResponse] for testing
type mockStreamServer struct {
	messages []string
	ctx      context.Context
}

func newMockStreamServer() *mockStreamServer {
	return &mockStreamServer{
		messages: make([]string, 0),
		ctx:      context.Background(),
	}
}

func (m *mockStreamServer) Send(resp *k8shelldpb.InitResponse) error {
	if msg := resp.GetMessage(); msg != "" {
		m.messages = append(m.messages, msg)
	}
	return nil
}

func (m *mockStreamServer) SetHeader(md metadata.MD) error  { return nil }
func (m *mockStreamServer) SendHeader(md metadata.MD) error { return nil }
func (m *mockStreamServer) SetTrailer(md metadata.MD)       {}
func (m *mockStreamServer) Context() context.Context        { return m.ctx }
func (m *mockStreamServer) SendMsg(msg interface{}) error   { return nil }
func (m *mockStreamServer) RecvMsg(msg interface{}) error   { return nil }

// setupTestEnvironment creates a temporary directory with test scripts
func setupTestEnvironment(t *testing.T) (string, func()) {
	t.Helper()

	// Create temporary directory for test
	tmpDir, err := os.MkdirTemp("", "k8shelld_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return tmpDir, cleanup
}

// createTestScript creates a test script file with given content
func createTestScript(t *testing.T, dir, name, content string) {
	t.Helper()

	scriptPath := filepath.Join(dir, name)
	err := os.WriteFile(scriptPath, []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test script %s: %v", name, err)
	}

	// Explicitly set execute permissions
	err = os.Chmod(scriptPath, 0755)
	if err != nil {
		t.Fatalf("Failed to set execute permissions on script %s: %v", name, err)
	}

	// Log the script creation for debugging
	t.Logf("Created test script: %s with permissions 0755", scriptPath)
}

// createTestInitService creates a test InitServiceServer
func createTestInitService(t *testing.T, homeDir string) *InitServiceServer {
	t.Helper()

	// Get current user's actual UID and GID for testing
	currentUID := os.Getuid()
	currentGID := os.Getgid()

	// Get current user's username to avoid lookup issues
	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("Failed to get current user: %v", err)
	}

	// Create a mock user with current user's credentials
	testUser := User{
		Username: currentUser.Username,
		Fullname: "Test User",
		Uid:      currentUID,
		Gid:      currentGID,
		Shell:    "/bin/bash",
		HomeDir:  homeDir,
	}

	// Create a mock GRPCApiService
	grpcApi := &GRPCApiService{
		user: testUser,
	}

	// Create logger that writes to test output
	logger := log.NewLogger("init-scripts")

	service := &InitServiceServer{
		grpcApi:    grpcApi,
		user:       testUser,
		scriptsDir: homeDir,
		logger:     logger,
	}

	return service
}

func TestRunInitScripts(t *testing.T) {
	tmpDir, cleanup := setupTestEnvironment(t)
	defer cleanup()

	t.Logf("Test directory: %s", tmpDir)

	// Create test foreground scripts
	createTestScript(t, tmpDir, "__init_fg_script1", "#!/bin/bash\necho 'Script 1 executed'\necho $TEST_VAR\nexit 0")
	createTestScript(t, tmpDir, "__init_fg_script2", "#!/bin/bash\necho 'Script 2 executed'\nexit 0")
	createTestScript(t, tmpDir, "__init_bg_script3__bg", "#!/bin/bash\necho 'BG Script 3 executed'\nexit 0")
	createTestScript(t, tmpDir, "__init_bg_script4__bg", "#!/bin/bash\necho 'BG Script 4 executed'\nexit 0")

	service := createTestInitService(t, tmpDir)
	mockStream := newMockStreamServer()

	req := &k8shelldpb.InitRequest{
		SetEnvVars: []string{"TEST_VAR=test_value"},
	}

	t.Logf("Running RunInitScripts with scriptsDir: %s", service.scriptsDir)
	err := service.RunInitScripts(req, mockStream)

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	t.Logf("Received %d messages: %v", len(mockStream.messages), mockStream.messages)
}
