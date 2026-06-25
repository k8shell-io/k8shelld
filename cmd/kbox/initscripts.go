// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/internal/display"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/spf13/cobra"
)

var InitCmd = &cobra.Command{
	Use:   "init-status",
	Short: "Show the status of workspace init scripts",
	Long: `Show the status of workspace init scripts.

By default a single snapshot of all scripts is printed and the command exits.

Use --progress to watch live progress with animated spinners until all scripts
have finished.  Press Ctrl+C at any time to exit the display early; running
scripts are not interrupted.

Use --errors to show only scripts that completed with an error.`,

	RunE: func(cmd *cobra.Command, args []string) error {
		noAnsi, _ := cmd.Flags().GetBool("no-ansi")
		progress, _ := cmd.Flags().GetBool("progress")
		errorsOnly, _ := cmd.Flags().GetBool("errors")

		// filterStates applies the --errors flag.
		filterStates := func(states []models.InitScriptState) []models.InitScriptState {
			if !errorsOnly {
				return states
			}
			out := states[:0:0]
			for _, s := range states {
				if s.HasError {
					out = append(out, s)
				}
			}
			return out
		}

		// --progress: live polling with animated spinner until all done or Ctrl+C.
		if progress {
			states, err := client.GetInitScripts()
			if err != nil {
				return fmt.Errorf("init: %w", err)
			}

			// Check if all scripts are already done before starting the live loop.
			allDoneAlready := true
			for _, s := range states {
				if s.Status == models.InitScriptPending || s.Status == models.InitScriptRunning {
					allDoneAlready = false
					break
				}
			}

			if !allDoneAlready {
				sigCh := make(chan os.Signal, 1)
				signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
				defer signal.Stop(sigCh)

				ticker := time.NewTicker(200 * time.Millisecond)
				defer ticker.Stop()

				var lastLineCount int
				spinTick := 0

				renderFrame := func(final bool) {
					spin := ""
					if !final {
						spin = display.SpinnerFrame(spinTick)
						spinTick++
					}
					if lastLineCount > 0 {
						display.ClearLines(os.Stdout, lastLineCount)
					}
					// Show the header only while scripts are still running.
					lines := display.RenderInitProgress(filterStates(states), spin, !noAnsi, true)
					for _, l := range lines {
						fmt.Println(l)
					}
					lastLineCount = len(lines)
				}

				renderFrame(false)

			loop:
				for {
					select {
					case <-sigCh:
						return nil
					case <-ticker.C:
						states, err = client.GetInitScripts()
						if err != nil {
							continue
						}
						done := true
						for _, s := range states {
							if s.Status == models.InitScriptPending || s.Status == models.InitScriptRunning {
								done = false
								break
							}
						}
						renderFrame(done)
						if done {
							break loop
						}
					}
				}

				return nil
			}

			// All already done — fall through to the static snapshot below.
			states, _ = client.GetInitScripts()
			filtered := filterStates(states)
			if len(filtered) == 0 {
				if errorsOnly {
					fmt.Println("No init script errors.")
				} else {
					fmt.Println("No init scripts registered.")
				}
				return nil
			}
			lines := display.RenderInitProgress(filtered, "", !noAnsi, false)
			for _, l := range lines {
				fmt.Println(l)
			}
			return nil
		}

		// Default: single static snapshot.
		states, err := client.GetInitScripts()
		if err != nil {
			return fmt.Errorf("init: %w", err)
		}
		filtered := filterStates(states)
		if len(filtered) == 0 {
			if errorsOnly {
				fmt.Println("No init script errors.")
			} else {
				fmt.Println("No init scripts registered.")
			}
			return nil
		}
		lines := display.RenderInitProgress(filtered, "", !noAnsi, false)
		for _, l := range lines {
			fmt.Println(l)
		}
		return nil
	},
}

func init() {
	InitCmd.Flags().Bool("no-ansi", false, "Disable ANSI colour codes and spinner")
	InitCmd.Flags().Bool("progress", false, "Watch live progress until all scripts finish")
	InitCmd.Flags().Bool("errors", false, "Show only scripts that completed with an error")

}
