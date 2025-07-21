package main

import (
	"fmt"
	"os"

	"github.com/k8shell-io/k8shelld/internal/common"
	"github.com/spf13/cobra"
)

var version = "0.0.0"
var commit_id = "0000000"
var socketPath string

var kboxCmd = &cobra.Command{
	Use:   "kbox",
	Short: "kbox is a set of tools for k8shell system operations",
	Long:  `kbox is a set of tools for k8shell system operations.`,
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

func init() {
	kboxCmd.SilenceUsage = true
	kboxCmd.SilenceErrors = true
	kboxCmd.CompletionOptions.DisableDefaultCmd = true
	kboxCmd.SetHelpCommand(&cobra.Command{
		Use:    "no-help",
		Hidden: true,
	})

	kboxCmd.AddCommand(DockerCmd)
	kboxCmd.AddCommand(ToolsInitCmd)
	kboxCmd.AddCommand(UptimeCmd)
	kboxCmd.AddCommand(ChannelsCmd)

	kboxCmd.PersistentFlags().StringVar(&socketPath, "socket", common.DefaultRESTAPIUnixSocket,
		"k8shelld unix socket path")
	kboxCmd.PersistentFlags().BoolP("version", "v", false, "Show version and exit")

	kboxCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		versionFlag, _ := cmd.Flags().GetBool("version")
		if versionFlag {
			fmt.Printf("kbox version: %s (commit: %s)\n", version, commit_id)
			os.Exit(0)
		}
		common.DefaultRESTAPIUnixSocket = socketPath
	}

}

func main() {
	if err := kboxCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
