package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/pkg/api"
	"github.com/spf13/cobra"
)

var (
	pretty bool
	since  bool
)

func init() {
	UptimeCmd.Flags().BoolVarP(&pretty, "pretty", "p", false, "Show uptime in pretty format")
	UptimeCmd.Flags().BoolVarP(&since, "since", "s", false, "System up since")
}

// uptimeCmd represents the uptime command
var UptimeCmd = &cobra.Command{
	Use:   "uptime",
	Short: "Display workspace uptime",
	Long:  "Display workspace uptime.",

	Run: func(cmd *cobra.Command, args []string) {
		resp, err := client.MakeRequest("GET", "/sysinfo", nil, nil)
		if err != nil {
			fmt.Println("Error fetching uptime:", err)
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		var data api.SystemInfo
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			fmt.Println("Error parsing response:", err)
			return
		}

		startTime, err := time.Parse(time.RFC3339, data.System.Uptime)
		if err != nil {
			fmt.Println("Error parsing uptime:", err)
			return
		}

		var output string

		if pretty {
			output = formatDuration(time.Since(startTime))
		} else if since {
			output = startTime.Format("2006-01-02 15:04:05")
		} else {
			output = fmt.Sprintf("%s, %d users, load average: %.2f, %.2f, %.2f", formatUptimeClassic(startTime),
				data.System.Users, data.System.CPUAvg1Min, data.System.CPUAvg5Min, data.System.CPUAvg15Min)
		}

		println(output)
	},
}

// formatDuration returns a human-readable duration (e.g. "2 days, 3 hours, 10 minutes")
func formatDuration(d time.Duration) string {
	days := d / (24 * time.Hour)
	d %= 24 * time.Hour
	hours := d / time.Hour
	d %= time.Hour
	minutes := d / time.Minute
	d %= time.Minute
	seconds := d / time.Second

	parts := []string{}

	if days > 0 {
		if days == 1 {
			parts = append(parts, "1 day")
		} else {
			parts = append(parts, fmt.Sprintf("%d days", days))
		}
	}
	if hours > 0 {
		if hours == 1 {
			parts = append(parts, "1 hour")
		} else {
			parts = append(parts, fmt.Sprintf("%d hours", hours))
		}
	}
	if minutes > 0 {
		if minutes == 1 {
			parts = append(parts, "1 minute")
		} else {
			parts = append(parts, fmt.Sprintf("%d minutes", minutes))
		}
	}
	if seconds == 1 {
		parts = append(parts, "1 second")
	} else {
		parts = append(parts, fmt.Sprintf("%d seconds", seconds))
	}

	return strings.Join(parts, ", ")
}

// formatUptimeClassic formats the uptime in a classic uptime format
func formatUptimeClassic(startTime time.Time) string {
	now := time.Now()
	uptime := now.Sub(startTime)
	currentTime := now.Format("15:04:05")

	days := int(uptime.Hours()) / 24
	hours := int(uptime.Hours()) % 24
	minutes := int(uptime.Minutes()) % 60

	var uptimeStr string
	if days > 0 {
		uptimeStr = fmt.Sprintf("up %d days, %2d:%02d", days, hours, minutes)
	} else {
		uptimeStr = fmt.Sprintf("up %d:%02d", hours, minutes)
	}

	return fmt.Sprintf("%s %s", currentTime, uptimeStr)
}
