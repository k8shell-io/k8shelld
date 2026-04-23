package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/internal/table"
	"github.com/spf13/cobra"
)

func rowColorFunc(row map[string]interface{}) string {
	if row["status"] == "STOPPED" {
		return "\033[2m" // Dimmed/shaded
	}
	if row["status"] == "DETACHED" {
		return "\033[33m" // Yellow
	}
	return ""
}

var tableFields = []table.FieldDefinition{
	{
		Name:  "id",
		Width: 17,
	},
	{
		Name:        "created",
		Width:       15,
		DisplayFunc: table.DisplayDateTime,
		Type:        table.Time,
		Format:      time.RFC3339,
	},
	{
		Name:        "duration",
		Width:       10,
		DisplayFunc: table.DisplayDuration,
	},
	{
		Name:  "status",
		Width: 8,
	},
	{
		Name:        "bytes_in",
		Type:        table.Int,
		Width:       8,
		DisplayFunc: table.DisplayBytes,
	},
	{
		Name:        "bytes_out",
		Type:        table.Int,
		Width:       8,
		DisplayFunc: table.DisplayBytes,
	},
	{
		Name:  "params",
		Width: 200,
	},
}

var StreamsCmd = &cobra.Command{
	Use:   "streams",
	Short: "Display streams",
	Long: `Display streams created from the client (ssh-proxy or api-server).

Stream IDs are prefixed with a short type code:

  sh-  Shell          Interactive PTY shell session
  ws-  WebSocket      Shell terminal session over WebSocket
  pf-  Port forward   TCP port-forwarding tunnel
  ux-  Unix socket    Unix domain socket proxy
  ex-  Exec           Arbitrary command execution or SFTP session
  rp-  Reverse proxy  Reverse proxy tunnel

The command displays the following fields:
  - id:            Stream ID (prefixed with type code)
  - created:       Stream creation time
  - duration:      Stream duration
  - status:        Stream status (ACTIVE, DETACHED, STOPPED)
  - bytes_in:      Bytes received
  - bytes_out:     Bytes sent
  - params:        Stream parameters (for DETACHED shell sessions includes ttl=<remaining>)

Use the --sort flag to sort the output by one or more fields. Prefix the field name with '-' for descending order.`,

	Run: func(cmd *cobra.Command, args []string) {
		url := "/streams"
		resp, err := client.MakeRequest("GET", url, nil, nil)
		if err != nil {
			fmt.Printf("Failed to get GRPC status: %v\n", err)
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		bodyBytes, _ := io.ReadAll(resp.Body)

		var t *table.Table
		if noAnsi, _ := cmd.Flags().GetBool("no-ansi"); noAnsi {
			t = table.NewTable(tableFields, nil)
		} else {
			t = table.NewTable(tableFields, rowColorFunc)
		}

		// Parse the JSON data
		err = t.ParseJsonData(bodyBytes)
		if err != nil {
			fmt.Printf("Error parsing JSON data: %v\n", err)
			return
		}

		// Sort the table
		sort := cmd.Flag("sort").Value.String()
		err = t.Sort(strings.Split(sort, ","))
		if err != nil {
			fmt.Printf("Error sorting table: %v\n", err)
			return
		}

		// Display the table
		if json, _ := cmd.Flags().GetBool("json"); json {
			if err := t.DisplayJSON(); err != nil {
				fmt.Printf("Error displaying JSON: %v\n", err)
			}
			return
		}

		if err := t.DisplayTable(); err != nil {
			fmt.Printf("Error displaying table: %v\n", err)
		}
	},
}

func init() {
	StreamsCmd.Flags().String("sort", "-created", "Comma separated list of fields to sort by, prefix with '-' for descending order")
	StreamsCmd.Flags().Bool("json", false, "Display output in JSON format")
	StreamsCmd.Flags().Bool("no-ansi", false, "Disable ANSI color output")
}
