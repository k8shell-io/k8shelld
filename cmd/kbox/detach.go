package main

import (
	"fmt"
	"os"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

var DetachCmd = &cobra.Command{
	Use:   "detach",
	Short: "Detach from the current shell session (keeps the process alive)",
	Long: `Detach from the current k8shell PTY session.

The shell process continues running in the background. Use 'kbox attach' to
reconnect later. The session ID is read from the K8SHELL_SESSION_ID environment
variable set automatically when a PTY session starts.`,

	Run: func(cmd *cobra.Command, args []string) {
		id := os.Getenv("K8SHELL_SESSION_ID")
		if id == "" {
			fmt.Fprintln(os.Stderr, "K8SHELL_SESSION_ID is not set — not inside a k8shell PTY session")
			os.Exit(1)
		}

		resp, err := client.MakeRequest("POST", "/shells/"+id+"/detach", nil, nil)
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
