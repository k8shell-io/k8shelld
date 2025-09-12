package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/k8shell-io/common/models"
	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

func init() {
}

var LastCmd = &cobra.Command{
	Use:   "last",
	Short: "Display last user sessions",
	Long:  "Display last user sessions.",

	Run: func(cmd *cobra.Command, args []string) {
		resp, err := client.MakeRequest("GET", "/sessions?num=50", nil, nil)
		if err != nil {
			fmt.Println("Error fetching last sessions: ", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			fmt.Printf("Server returned error: %s\n", resp.Status)
			return
		}

		sessions := []models.SSHSession{}
		decoder := json.NewDecoder(resp.Body)
		if err := decoder.Decode(&sessions); err != nil {
			fmt.Println("Error decoding response: ", err)
			return
		}

		for _, session := range sessions {
			formatLastEntry(session)
		}
	},
}

func formatLastEntry(session models.SSHSession) {
	if session.StartTime == nil {
		fmt.Printf("Error: start time is nil\n")
		return
	}
	startTime := *session.StartTime

	var endTime time.Time
	var isStillLoggedIn bool

	if session.EndTime != nil {
		endTime = *session.EndTime
	} else {
		isStillLoggedIn = true
	}

	username := fmt.Sprintf("%-8s", session.Username)
	terminal := "pts/0   "
	clientIP := fmt.Sprintf("%-16s", session.ClientIP)
	startFormatted := startTime.Format("Mon Jan _2 15:04")

	if isStillLoggedIn {
		fmt.Printf("%s %s %s %s   still logged in\n",
			username, terminal, clientIP, startFormatted)
	} else {
		endFormatted := endTime.Format("15:04")

		duration := endTime.Sub(startTime)
		hours := int(duration.Hours())
		minutes := int(duration.Minutes()) % 60
		durationFormatted := fmt.Sprintf("(%02d:%02d)", hours, minutes)

		if startTime.Format("2006-01-02") == endTime.Format("2006-01-02") {
			fmt.Printf("%s %s %s %s - %s  %s\n",
				username, terminal, clientIP, startFormatted, endFormatted, durationFormatted)
		} else {
			endFormattedFull := endTime.Format("Mon Jan _2 15:04")
			fmt.Printf("%s %s %s %s - %s  %s\n",
				username, terminal, clientIP, startFormatted, endFormattedFull, durationFormatted)
		}
	}
}
