package main

import (
	"encoding/json"
	"fmt"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/pkg/api"
	"github.com/spf13/cobra"
)

var SplashCmd = &cobra.Command{
	Use:   "splash",
	Short: "Display the workspace splash message",
	Long:  "Display the workspace splash message.",

	Run: func(cmd *cobra.Command, args []string) {
		resp, err := client.MakeRequest("GET", "/splash", nil, nil)
		if err != nil {
			fmt.Println("Error fetching splash:", err)
			return
		}
		defer resp.Body.Close()

		if err := client.CheckApplicationError(resp); err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		var data api.SplashInfo
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			fmt.Printf("Error parsing response: %v\n", err)
			return
		}

		fmt.Println(data.Text)
	},
}
