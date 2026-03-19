package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/pkg/api"
	"github.com/spf13/cobra"
)

var identityJSON bool

func init() {
	IdentityCmd.Flags().BoolVar(&identityJSON, "json", false, "Output JSON (pretty-printed)")
}

var IdentityCmd = &cobra.Command{
	Use:   "identity",
	Short: "Display workspace identity claims",
	Long:  "Display the JWT identity claims for the current workspace user.",

	Run: func(cmd *cobra.Command, args []string) {
		resp, err := client.MakeRequest("GET", "/identity", nil, nil)
		if err != nil {
			fmt.Println("Error fetching identity:", err)
			return
		}
		defer resp.Body.Close()

		if err := client.CheckApplicationError(resp); err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		if identityJSON {
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Printf("Error reading response: %v\n", err)
				return
			}
			var buf bytes.Buffer
			if err := json.Indent(&buf, raw, "", "  "); err != nil {
				fmt.Println(string(raw))
				return
			}
			fmt.Println(buf.String())
			return
		}

		var data api.IdentityInfo
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			fmt.Printf("Error parsing response: %v\n", err)
			return
		}

		sudo := "no"
		if data.Sudo {
			sudo = "yes"
		}

		roles := strings.Join(data.Roles, ", ")
		if roles == "" {
			roles = "none"
		}

		rows := [][2]string{
			{"Username", strOr(data.Username, "n/a")},
			{"Name", strOr(data.Name, "n/a")},
			{"Email", strOr(data.Email, "n/a")},
			{"UID", fmt.Sprintf("%d", data.UID)},
			{"GID", fmt.Sprintf("%d", data.GID)},
			{"Shell", data.Shell},
			{"Sudo", sudo},
			{"Roles", roles},
		}
		if data.Organization != "" {
			rows = append(rows, [2]string{"Organization", data.Organization})
		}
		if data.Source != "" {
			rows = append(rows, [2]string{"Source", data.Source})
		}
		rows = append(rows, [2]string{"Expires", strOr(data.ExpiresAt, "n/a")})

		printGroup("Identity", rows)
	},
}

func strOr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
