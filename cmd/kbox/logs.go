package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	component  string
	level      string
	jsonFormat bool
	wrapLines  bool
	followLogs bool
	noAnsi     bool
	lastN      int
)

func init() {
	LogsCmd.Flags().StringVarP(&component, "component", "c", "", "Filter logs by component")
	LogsCmd.Flags().StringVarP(&level, "level", "l", "", "Filter logs by level (debug, info, warn, error, fatal)")
	LogsCmd.Flags().BoolVarP(&jsonFormat, "json", "j", false, "Output logs in JSON format")
	LogsCmd.Flags().BoolVar(&noAnsi, "no-ansi", false, "Disable ansi colors in output")
	LogsCmd.Flags().BoolVarP(&wrapLines, "wrap", "w", false, "Wrap long lines instead of truncating")
	LogsCmd.Flags().BoolVarP(&followLogs, "follow", "f", false, "Follow log output in real time")
	LogsCmd.Flags().IntVarP(&lastN, "last-n", "n", 0, "Display the last N log entries")
}

var LogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Display workspace logs",
	Long:  "Display workspace logs. Use -f to follow in real time.",

	Run: func(cmd *cobra.Command, args []string) {
		// validation
		if level != "" {
			level = strings.ToLower(level)
			if level != "debug" && level != "info" && level != "warn" &&
				level != "error" && level != "fatal" {
				fmt.Printf("Invalid log level: %s. Valid levels are: debug, info, warn, error, fatal\n", level)
				return
			}
		}

		// query string
		queryString := ""
		if component != "" {
			queryString += fmt.Sprintf("component=%s&", component)
		}
		if level != "" {
			queryString += fmt.Sprintf("level=%s&", level)
		}
		if followLogs {
			queryString += "follow=true&"
		}
		if lastN > 0 {
			queryString += fmt.Sprintf("lastN=%d&", lastN)
		}
		if queryString != "" {
			queryString = "?" + strings.TrimSuffix(queryString, "&")
		}

		// request logs
		resp, err := client.MakeRequest("GET", "/logs"+queryString, nil, nil)
		if err != nil {
			fmt.Println("Error fetching logs:", err)
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		if noAnsi {
			color.NoColor = true
		}

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if jsonFormat {
				fmt.Println(line)
				continue
			}

			var entry LogEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}
			printColoredLog(entry)
		}

		if err := scanner.Err(); err != nil {
			fmt.Printf("Error reading logs: %v\n", err)
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
	line := fmt.Sprintf("%s %s %s component=%s\n", timestamp, coloredLevel, entry.Message, entry.Component)

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
