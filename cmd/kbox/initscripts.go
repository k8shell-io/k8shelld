package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/internal/display"
	"github.com/spf13/cobra"
)

var InitScriptsCmd = &cobra.Command{
	Use:   "initscripts",
	Short: "Show the progress of workspace init scripts",
	Long: `Show the progress of workspace init scripts.

Each script is listed with its current state:
  pending   — not yet started
  running   — currently executing (animated spinner)
  done      — completed (tick for success, cross for failure) with elapsed time

By default the command polls until all scripts have finished.
Press Ctrl+C at any time to exit the display early; running scripts
are not interrupted.`,

	RunE: func(cmd *cobra.Command, args []string) error {
		noAnsi, _ := cmd.Flags().GetBool("no-ansi")
		noWait, _ := cmd.Flags().GetBool("no-wait")

		// One-shot snapshot mode.
		if noWait {
			states, err := client.GetInitScripts()
			if err != nil {
				return fmt.Errorf("initscripts: %w", err)
			}
			if len(states) == 0 {
				fmt.Println("No init scripts registered.")
				return nil
			}
			lines := display.RenderInitProgress(states, "", !noAnsi)
			for _, l := range lines {
				fmt.Println(l)
			}
			return nil
		}

		// Polling mode: update in-place until all done or Ctrl+C.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()

		var lastLineCount int
		spinTick := 0

		// Initial fetch and render before the first tick so the output appears
		// immediately rather than after 200 ms.
		states, err := client.GetInitScripts()
		if err != nil {
			return fmt.Errorf("initscripts: %w", err)
		}
		if len(states) == 0 {
			fmt.Println("No init scripts registered.")
			return nil
		}

		renderFrame := func(final bool) {
			spin := ""
			if !final {
				spin = display.SpinnerFrame(spinTick)
				spinTick++
			}
			if lastLineCount > 0 {
				display.ClearLines(os.Stdout, lastLineCount)
			}
			lines := display.RenderInitProgress(states, spin, !noAnsi)
			for _, l := range lines {
				fmt.Println(l)
			}
			lastLineCount = len(lines)
		}

		renderFrame(false)

		for {
			select {
			case <-sigCh:
				// Exit display; do not cancel scripts.
				return nil

			case <-ticker.C:
				states, err = client.GetInitScripts()
				if err != nil {
					// Server may not yet have the endpoint; just keep waiting.
					continue
				}
				allDone := true
				for _, s := range states {
					if s.Status == "pending" || s.Status == "running" {
						allDone = false
						break
					}
				}
				renderFrame(allDone)
				if allDone {
					return nil
				}
			}
		}
	},
}

func init() {
	InitScriptsCmd.Flags().Bool("no-ansi", false, "Disable ANSI colour codes and spinner")
	InitScriptsCmd.Flags().Bool("no-wait", false, "Print a one-shot snapshot and exit immediately")
}
