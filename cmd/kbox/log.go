package cmd

import (
	"fmt"
	"tools/kbox/k8shelld"

	"github.com/spf13/cobra"
)

var (
	logLevel   string
	logMessage string
	logComp    string
)

var LogCmd = &cobra.Command{
	Use:   "log",
	Short: "Log a message",
	Long:  "Log a message to the k8shell logging system",
	Run: func(cmd *cobra.Command, args []string) {
		logger := k8shelld.NewLogger(logComp)
		if logLevel == "I" {
			logger.Info("%s", logMessage)
		} else if logLevel == "D" {
			logger.Debug("%s", logMessage)
		} else if logLevel == "W" {
			logger.Warn("%s", logMessage)
		} else if logLevel == "E" {
			logger.Error("%s", logMessage)
		}
	},
}

func init() {
	LogCmd.Flags().BoolP("info", "i", false, "Set log level to info")
	LogCmd.Flags().BoolP("debug", "d", false, "Set log level to debug")
	LogCmd.Flags().BoolP("warning", "w", false, "Set log level to warning")
	LogCmd.Flags().BoolP("error", "e", false, "Set log level to error")
	LogCmd.Flags().StringVarP(&logMessage, "message", "m", "", "Message to log")
	LogCmd.Flags().StringVarP(&logComp, "comp", "c", "info", "Component name")
	LogCmd.MarkFlagRequired("message")
	LogCmd.PreRunE = validateExclusiveFlags
}

func validateExclusiveFlags(cmd *cobra.Command, args []string) error {
	info, _ := cmd.Flags().GetBool("info")
	debug, _ := cmd.Flags().GetBool("debug")
	warning, _ := cmd.Flags().GetBool("warning")
	errorFlag, _ := cmd.Flags().GetBool("error")
	logLevel = "I"

	flagCount := 0
	if info {
		logLevel = "I"
		flagCount++
	}
	if debug {
		logLevel = "D"
		flagCount++
	}
	if warning {
		logLevel = "W"
		flagCount++
	}
	if errorFlag {
		logLevel = "E"
		flagCount++
	}
	if flagCount > 1 {
		return fmt.Errorf("only one log level flag is allowed")
	}

	return nil
}
