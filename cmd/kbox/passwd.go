// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var PasswdCmd = &cobra.Command{
	Use:   "passwd",
	Short: "Change the workspace user's password",
	Long: `Change the workspace user's password.

When run under sudo, the current password is not required. Otherwise you
must confirm your current password before a new one is accepted.`,

	Run: func(cmd *cobra.Command, args []string) {
		fd := int(os.Stdin.Fd())

		var currentPassword string
		if os.Getenv("SUDO_USER") == "" {
			pw, err := readPassword(fd, "Current password: ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "passwd: %v\n", err)
				os.Exit(1)
			}
			currentPassword = pw
		}

		newPassword, err := readPassword(fd, "New password: ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "passwd: %v\n", err)
			os.Exit(1)
		}
		confirmPassword, err := readPassword(fd, "Retype new password: ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "passwd: %v\n", err)
			os.Exit(1)
		}
		if newPassword != confirmPassword {
			fmt.Fprintln(os.Stderr, "passwd: passwords do not match")
			os.Exit(1)
		}

		body, _ := json.Marshal(struct {
			Password        string `json:"password"`
			CurrentPassword string `json:"currentPassword,omitempty"`
		}{Password: newPassword, CurrentPassword: currentPassword})

		resp, err := client.MakeRequest("PUT", "/password",
			map[string]string{"Content-Type": "application/json"}, bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "passwd: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if err := client.CheckApplicationError(resp); err != nil {
			fmt.Fprintf(os.Stderr, "passwd: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("passwd: password updated successfully")
	},
}

// readPassword prints prompt, reads a line of input from fd with echo
// disabled, and returns it. It rejects empty input.
func readPassword(fd int, prompt string) (string, error) {
	fmt.Print(prompt)
	b, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("failed to read password: %w", err)
	}
	if len(b) == 0 {
		return "", fmt.Errorf("password must not be empty")
	}
	return string(b), nil
}
