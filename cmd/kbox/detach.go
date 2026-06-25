// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

var DetachCmd = &cobra.Command{
	Use:   "detach",
	Short: "Detach from the current shell session (keeps the process alive)",
	Long: `Detach from the current k8shell PTY session.

The shell process continues running in the background. Use 'kbox attach' to
reconnect later. The session ID is read from the K8SHELL_SESSION_ID environment
variable set automatically when a PTY session starts.

Use --ttl to override how long the detached session survives before being
garbage-collected (e.g. "30m", "1h", "0" for no expiry). The server will
reject values that exceed its configured maximum.`,

	Run: func(cmd *cobra.Command, args []string) {
		id := os.Getenv("K8SHELL_SESSION_ID")
		if id == "" {
			fmt.Fprintln(os.Stderr, "K8SHELL_SESSION_ID is not set — not inside a k8shell PTY session")
			os.Exit(1)
		}

		// Build optional JSON body with TTL override.
		var bodyReader io.Reader
		if ttlStr, _ := cmd.Flags().GetString("ttl"); ttlStr != "" {
			if ttlStr == "0" {
				ttlStr = "0s" // time.ParseDuration requires a unit
			}
			if _, err := time.ParseDuration(ttlStr); err != nil {
				fmt.Fprintf(os.Stderr, "detach: invalid --ttl %q: %v\n", ttlStr, err)
				os.Exit(1)
			}
			body, _ := json.Marshal(struct {
				TTL string `json:"ttl"`
			}{ttlStr})
			bodyReader = bytes.NewReader(body)
		}

		resp, err := client.MakeRequest("POST", "/shells/"+id+"/detach", nil, bodyReader)
		if err != nil {
			fmt.Fprintf(os.Stderr, "detach: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if err := client.CheckApplicationError(resp); err != nil {
			fmt.Fprintf(os.Stderr, "detach: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("Detached. Session is still running in the background.")
		fmt.Printf("To reattach: kbox attach %s\n", id)
	},
}

func init() {
	DetachCmd.Flags().String("ttl", "", `Override the detached session TTL (e.g. "30m", "1h", "0" for no expiry; default: use server config)`)
}
