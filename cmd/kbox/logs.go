package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type LogEntry struct {
	Level     string `json:"level"`
	Component string `json:"component"`
	Time      string `json:"time"`
	Message   string `json:"message"`
}

var (
	wrapLines  bool
	followLogs bool
)

func init() {
	LogsCmd.Flags().BoolVarP(&wrapLines, "wrap", "w", false, "Wrap long lines instead of truncating")
	LogsCmd.Flags().BoolVarP(&followLogs, "follow", "f", false, "Follow log output in real time")
}

var LogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Display workspace logs",
	Long:  "Display workspace logs. By default, returns buffered logs. Use -f to follow in real time.",

	Run: func(cmd *cobra.Command, args []string) {
		resp, err := client.MakeRequest("GET", "/logs", nil, nil)
		if err != nil {
			fmt.Println("Error fetching logs:", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			fmt.Printf("Server returned error: %s\n", resp.Status)
			return
		}

		scanner := bufio.NewScanner(resp.Body)

		for scanner.Scan() {
			line := scanner.Text()
			var entry LogEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				fmt.Println(line) // fallback
				continue
			}
			printColoredLog(entry)
		}

		if err := scanner.Err(); err != nil {
			fmt.Printf("Error reading logs: %v\n", err)
		}

		// If not following, return after initial stream ends
		if !followLogs {
			return
		}

		// follow: keep reading even after disconnection
		resp, err = client.MakeRequest("GET", "/logs", nil, nil)
		if err != nil {
			fmt.Println("Error fetching logs for follow:", err)
			return
		}
		defer resp.Body.Close()

		scanner = bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			var entry LogEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				fmt.Println(line)
				continue
			}
			printColoredLog(entry)
		}
	},
}

func printColoredLog(entry LogEntry) {
	ts, err := time.Parse(time.RFC3339, entry.Time)
	timestamp := entry.Time
	if err == nil {
		timestamp = ts.Format("2006-01-02T15:04:05-07:00")
	}

	var levelColor *color.Color
	var levelShort string
	switch strings.ToLower(entry.Level) {
	case "debug":
		levelColor = color.New(color.FgHiBlack)
		levelShort = "DBG"
	case "info":
		levelColor = color.New(color.FgGreen)
		levelShort = "INF"
	case "warn", "warning":
		levelColor = color.New(color.FgYellow)
		levelShort = "WRN"
	case "error":
		levelColor = color.New(color.FgRed)
		levelShort = "ERR"
	case "fatal":
		levelColor = color.New(color.FgHiRed, color.Bold)
		levelShort = "FTL"
	default:
		levelColor = color.New(color.FgWhite)
		levelShort = strings.ToUpper(entry.Level)
	}

	coloredLevel := levelColor.Sprintf("%-3s", levelShort)
	line := fmt.Sprintf("%s %s %s component=%s", timestamp, coloredLevel, entry.Message, entry.Component)

	if wrapLines {
		fmt.Print(line)
	} else {
		terminalWidth := getTerminalWidth()
		if terminalWidth != 0 && len(line) > terminalWidth {
			line = line[:terminalWidth-1] + "…"
		}
		fmt.Print(line)
	}
}

func getTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0
	}
	return width + 9
}
