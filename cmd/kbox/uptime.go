package main

import (
	"fmt"
	"os"

	"github.com/k8shell-io/k8shelld/pkg/client"
	"github.com/spf13/cobra"
)

var (
	pretty bool
	since  bool
)

// uptimeCmd represents the uptime command
var UptimeCmd = &cobra.Command{
	Use:   "uptime",
	Short: "Display workspace uptime",
	Long:  "Display workspace uptime.",
	Run: func(cmd *cobra.Command, args []string) {
		queryParams := make(map[string]string)
		if pretty {
			queryParams["pretty"] = "true"
		}
		if since {
			queryParams["since"] = "true"
		}

		// Build the API URL with query parameters
		url := "uptime"
		if len(queryParams) > 0 {
			url += "?"
			for key, value := range queryParams {
				url += fmt.Sprintf("%s=%s&", key, value)
			}
			url = url[:len(url)-1] // Remove trailing '&'
		}

		response, err := client.MakeRequest("GET", url, map[string]string{
			"Accept": "text",
		})
		if err != nil {
			fmt.Printf("%v\n", err)
			os.Exit(1)
		}

		fmt.Println(response)
	},
}

func init() {
	UptimeCmd.Flags().BoolVarP(&pretty, "pretty", "p", false, "Show uptime in pretty format")
	UptimeCmd.Flags().BoolVarP(&since, "since", "s", false, "System up since")
}
