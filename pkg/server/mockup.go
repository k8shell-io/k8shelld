package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"time"
)

// CommandRunner is an interface for (mocked/pretended) running commands.
type CommandRunner interface {
	Run(ctx context.Context, cmd *exec.Cmd) ([]byte, error)
}

// RealCommandRunner is an implementation of the CommandRunner interface that runs commands in the real environment.
type RealCommandRunner struct{}

// Run executes the given command within the provided context and returns the output as a byte slice.
func (r *RealCommandRunner) Run(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	return cmd.CombinedOutput()
}

// MockCommandRunner is an implementation of the CommandRunner interface that pretends to run commands.
type MockCommandRunner struct {
	Output map[string]string
	Err    map[string]error
	Called map[string]int
}

// Run executes the given command within the provided context and returns the output as a byte slice.
func (m *MockCommandRunner) Run(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	m.Called[cmd.Path] += 1
	output, ok := m.Output[cmd.Path]
	if !ok {
		return nil, m.Err[cmd.Path]
	}
	return []byte(output), m.Err[cmd.Path]
}

// FileSystem defines an interface for filesystem operations.
type FileSystem interface {
	Stat(path string) (os.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	MkdirAll(path string, perm os.FileMode) error
	Rename(oldpath, newpath string) error
}

// RealFileSystem is an implementation of the FileSystem interface that interacts with the real filesystem.
type RealFileSystem struct{}

// Stat returns file information about the specified path.
func (r *RealFileSystem) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// ReadFile reads the contents of the file at the specified path.
func (r *RealFileSystem) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// WriteFile writes the specified data to the file at the specified path with the specified permissions.
func (r *RealFileSystem) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// MkdirAll creates a directory and any necessary parents at the specified path with the specified permissions.
func (r *RealFileSystem) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// Rename renames a file or directory from oldpath to newpath.
func (r *RealFileSystem) Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// MockFileSystem is an implementation of the FileSystem interface that pretends to interact with the filesystem.
type MockFileSystem struct {
	Files map[string][]byte
	Dirs  map[string]os.FileMode
}

// NewMockFileSystem creates a new instance of MockFileSystem.
func NewMockFileSystem() *MockFileSystem {
	return &MockFileSystem{
		Files: make(map[string][]byte),
		Dirs:  make(map[string]os.FileMode),
	}
}

// Stat returns file information about the specified path.
func (fs *MockFileSystem) Stat(path string) (os.FileInfo, error) {
	if _, exists := fs.Files[path]; exists {
		return &MockFileInfo{}, nil
	}
	return nil, fmt.Errorf("file %s not found: %w", path, os.ErrNotExist)
}

// ReadFile reads the contents of the file at the specified path.
func (fs *MockFileSystem) ReadFile(path string) ([]byte, error) {
	if data, exists := fs.Files[path]; exists {
		return data, nil
	}
	return nil, fmt.Errorf("file not found: %s", path)
}

// WriteFile writes the specified data to the file at the specified path with the specified permissions.
func (fs *MockFileSystem) WriteFile(path string, data []byte, perm os.FileMode) error {
	fs.Files[path] = data
	return nil
}

// MkdirAll creates a directory and any necessary parents at the specified path with the specified permissions.
func (fs *MockFileSystem) MkdirAll(path string, perm os.FileMode) error {
	fs.Dirs[path] = perm
	return nil
}

// Rename renames a file or directory from oldpath to newpath.
func (fs *MockFileSystem) Rename(oldpath, newpath string) error {
	fs.Files[newpath] = fs.Files[oldpath]
	delete(fs.Files, oldpath)
	return nil
}

// MockFileInfo satisfies os.FileInfo interface (for tests)
type MockFileInfo struct{}

func (m *MockFileInfo) Name() string       { return "mockfile" }
func (m *MockFileInfo) Size() int64        { return 0 }
func (m *MockFileInfo) Mode() os.FileMode  { return 0644 }
func (m *MockFileInfo) ModTime() time.Time { return time.Now() }
func (m *MockFileInfo) IsDir() bool        { return false }
func (m *MockFileInfo) Sys() interface{}   { return nil }

// UserLookup is an interface for looking up user information.
type UserLookup interface {
	Lookup(username string) (*user.User, error)
	LookupId(uid string) (*user.User, error)
}

// RealUserLookup is an implementation of the UserLookup interface that interacts with the real user database.
type RealUserLookup struct{}

// Lookup returns the user information for the specified username.
func (r *RealUserLookup) Lookup(username string) (*user.User, error) {
	return user.Lookup(username)
}

// LookupId returns the user information for the specified UID.
func (r *RealUserLookup) LookupId(uid string) (*user.User, error) {
	return user.LookupId(uid)
}

// MockUserLookup is an implementation of the UserLookup interface that pretends to look up user information.
type MockUserLookup struct {
	Users map[string]*user.User
	UIDs  map[string]*user.User
}

// NewMockUserLookup creates a new instance of MockUserLookup.
func NewMockUserLookup() *MockUserLookup {
	return &MockUserLookup{
		Users: make(map[string]*user.User),
		UIDs:  make(map[string]*user.User),
	}
}

// Lookup returns the user information for the specified username.
func (m *MockUserLookup) Lookup(username string) (*user.User, error) {
	if u, ok := m.Users[username]; ok {
		return u, nil
	}
	return nil, fmt.Errorf("user not found")
}

// LookupId returns the user information for the specified UID.
func (m *MockUserLookup) LookupId(uid string) (*user.User, error) {
	if u, ok := m.UIDs[uid]; ok {
		return u, nil
	}
	return nil, fmt.Errorf("user not found")
}
