// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package main

import (
	"fmt"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

var shutdownDelete bool

func init() {
	ShutdownCmd.Flags().BoolVar(&shutdownDelete, "delete", false, "Delete the workspace (default action is stop)")
}

var ShutdownCmd = &cobra.Command{
	Use:   "shutdown",
	Short: "Shutdown the workspace",
	Long: `Shutdown the workspace gracefully.

Actions:
  --stop    Stop the workspace (default)
  --delete  Delete the workspace`,

	Run: func(cmd *cobra.Command, args []string) {
		action := "stop"
		if shutdownDelete {
			action = "delete"
		}

		resp, err := client.MakeRequest("POST", "/shutdown?action="+action, nil, nil)
		if err != nil {
			fmt.Println("Error shutting down workspace:", err)
			return
		}
		defer resp.Body.Close()

		if err := client.CheckApplicationError(resp); err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		if shutdownDelete {
			fmt.Println("Request to delete the workspace was submitted.")
		} else {
			fmt.Println("Request to stop the workspace was submitted.")
		}
	},
}
