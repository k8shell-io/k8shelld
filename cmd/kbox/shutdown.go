package main

import (
	"fmt"
	"net/http"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

func init() {
}

var ShutdownCmd = &cobra.Command{
	Use:   "shutdown",
	Short: "Shutdown the workspace",
	Long:  "Shutdown the workspace gracefully.",

	Run: func(cmd *cobra.Command, args []string) {
		resp, err := client.MakeRequest("POST", "/shutdown", nil, nil)
		if err != nil {
			fmt.Println("Error shutting down workspace: ", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			fmt.Printf("Server returned error: %s\n", resp.Status)
			return
		}

		fmt.Println("Workspace shut down successfully.")
	},
}
