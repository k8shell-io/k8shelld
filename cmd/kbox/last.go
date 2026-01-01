package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

var (
	lastNumLines   int = 15
	lastWideFormat bool
)

func init() {
	LastCmd.Flags().IntVarP(&lastNumLines, "num", "n", 15, "Number of sessions to display")
	LastCmd.Flags().BoolVarP(&lastWideFormat, "wide", "w", false, "Display output in wide format")
}

var LastCmd = &cobra.Command{
	Use:   "last",
	Short: "Display last user sessions",
	Long: `Display last user sessions.

The command displays the following fields:
- username
- terminal
- client IP
- login time
- logout time or "still logged in"
- duration of the session
- bytes received
- bytes sent
`,

	Run: func(cmd *cobra.Command, args []string) {
		resp, err := client.MakeRequest("GET", fmt.Sprintf("/sessions?num=%d", lastNumLines), nil, nil)
		if err != nil {
			fmt.Println("Error fetching last sessions: ", err)
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		sessions := []models.SSHSession{}
		decoder := json.NewDecoder(resp.Body)
		if err := decoder.Decode(&sessions); err != nil {
			fmt.Println("Error decoding response: ", err)
			return
		}

		for _, session := range sessions {
			formatLastEntry(session, lastWideFormat)
		}
	},
}

// formatBytes converts bytes to human readable format (K, M, G)
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func formatLastEntry(session models.SSHSession, wideFormat bool) {
	if session.StartTime == nil {
		fmt.Printf("Error: start time is nil\n")
		return
	}
	start := *session.StartTime

	var (
		end             time.Time
		isStillLoggedIn bool
	)
	if session.EndTime != nil {
		end = *session.EndTime
	} else {
		isStillLoggedIn = true
	}

	username := session.Username
	terminal := "pts/0"
	clientIP := session.ClientIP
	startStr := start.Format("Mon Jan _2 15:04")

	var timeCol string
	if isStillLoggedIn {
		timeCol = startStr + "   still logged in"
	} else {
		d := end.Sub(start)
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		dur := fmt.Sprintf("(%02d:%02d)", h, m)
		if wideFormat {
			s := int(d.Seconds()) % 60
			dur = fmt.Sprintf("(%02d:%02d:%02d)", h, m, s)
		}

		if start.Format("2006-01-02") == end.Format("2006-01-02") {
			timeCol = fmt.Sprintf("%s - %s  %s", startStr, end.Format("15:04"), dur)
		} else {
			timeCol = fmt.Sprintf("%s - %s  %s", startStr, end.Format("Mon Jan _2 15:04"), dur)
		}
	}

	if wideFormat {
		fmt.Printf("%-10s %-8s %-16s %-37s %7s %7s\n",
			username, terminal, clientIP, timeCol,
			formatBytes(session.BytesIn), formatBytes(session.BytesOut))
	} else {
		fmt.Printf("%-10s %-8s %-16s %-34s\n",
			username, terminal, clientIP, timeCol)
	}
}
