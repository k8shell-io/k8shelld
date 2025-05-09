package server

import (
	"context"
	"os/user"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

var testcases = []struct {
	name        string
	groupExists bool
	userExists  bool
	userSudo    bool
}{
	{"Main group exists", true, false, false},
	{"Main group does not exist", false, false, false},
	{"Main user exists", true, true, false},
	{"Main user does not exist", true, false, false},
	{"Main user exists with sudo", true, true, true},
	{"Main user does not exist with sudo", true, false, true},
}

func TestCreateMainUser(t *testing.T) {
	ctx := context.Background()

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			assert := assert.New(t)
			mainUser := MainUser{
				User: User{
					Username: "testuser",
					Fullname: "Test User",
					Uid:      1000,
					Gid:      1000,
				},
				HomeDir: "/home/testuser",
				Shell:   "/bin/bash",
				Sudo:    false,
			}
			if tc.userSudo {
				mainUser.Sudo = true
			}
			runner := &MockCommandRunner{
				Called: map[string]int{
					"/usr/sbin/groupadd": 0,
					"/usr/sbin/useradd":  0,
					"/usr/bin/cp":        0,
				},
				Output: map[string]string{
					"/usr/sbin/groupadd": "",
					"/usr/sbin/useradd":  "",
					"/usr/bin/cp":        "",
				},
				Err: map[string]error{},
			}
			fs := NewMockFileSystem()

			lookup := &MockUserLookup{
				Users: map[string]*user.User{},
				UIDs:  map[string]*user.User{},
			}
			if tc.userExists {
				lookup.Users["testuser"] = &user.User{
					Username: mainUser.Username,
					Uid:      strconv.Itoa(mainUser.Uid),
					Gid:      strconv.Itoa(mainUser.Gid),
					Name:     mainUser.Fullname,
					HomeDir:  mainUser.HomeDir,
				}
				lookup.UIDs["1000"] = lookup.Users["testuser"]
			}

			// Create the main group
			err := fs.WriteFile(groupFilePath, []byte(""), 0644)
			if tc.groupExists {
				err = fs.WriteFile(groupFilePath, []byte("testgroup:x:1000:"), 0644)
			}
			assert.NoError(err)

			// Act
			// Create the main user
			err = CreateMainUser(ctx, runner, fs, lookup, mainUser)

			// Assert
			assert.NoError(err)
			assert.Equal(tc.groupExists, runner.Called["/usr/sbin/groupadd"] == 0)
			assert.Equal(tc.userExists, runner.Called["/usr/sbin/useradd"] == 0)
		})
	}
}
func TestGroupExists(t *testing.T) {
	tests := []struct {
		name             string
		groupFileMissing bool
		groupFile        string
		nameOrGID        string
		expected         bool
		wantErr          bool
	}{
		{
			name:             "Group exists by name",
			groupFileMissing: false,
			groupFile:        "testgroup:x:1100:",
			nameOrGID:        "testgroup",
			expected:         true,
			wantErr:          false,
		},
		{
			name:             "Group exists by GID",
			groupFileMissing: false,
			groupFile:        "testgrouppp:x:1000:",
			nameOrGID:        "1000",
			expected:         true,
			wantErr:          false,
		},
		{
			name:             "Group does not exist",
			groupFileMissing: false,
			groupFile:        "othergroup:x:1001:",
			nameOrGID:        "testgroup",
			expected:         false,
			wantErr:          false,
		},
		{
			name:             "Empty group file",
			groupFileMissing: false,
			groupFile:        "",
			nameOrGID:        "testgroup",
			expected:         false,
			wantErr:          false,
		},
		{
			name:             "Invalid group file format",
			groupFileMissing: false,
			groupFile:        "invalidgroupfile",
			nameOrGID:        "testgroup",
			expected:         false,
			wantErr:          false,
		},
		{
			name:             "Very large group file",
			groupFileMissing: false,
			groupFile:        strings.Repeat("A", 1024*65),
			nameOrGID:        "testgroup",
			expected:         false,
			wantErr:          true,
		},
		{
			name:             "Group file missing",
			groupFileMissing: true,
			groupFile:        "",
			nameOrGID:        "testgroup",
			expected:         false,
			wantErr:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			fs := NewMockFileSystem()
			assert := assert.New(t)
			if !tt.groupFileMissing {
				err := fs.WriteFile(groupFilePath, []byte(tt.groupFile), 0644)
				assert.NoError(err)
			}

			// Act
			exists, err := groupExists(fs, tt.nameOrGID)

			// Assert
			if tt.wantErr {
				assert.Error(err)
			} else {
				assert.NoError(err)
			}
			assert.Equal(tt.expected, exists)
		})
	}
}
