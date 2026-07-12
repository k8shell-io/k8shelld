// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"fmt"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

var UserCmd = &cobra.Command{
	Use:   "user",
	Short: "Display user information",
	Long:  "Display information about the current workspace user from the profile resource.",
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
	},
}

var userNameCmd = &cobra.Command{
	Use:   "name",
	Short: "Print the user's full name",
	Run: func(cmd *cobra.Command, args []string) {
		data := fetchProfile()
		if data == nil {
			return
		}
		fmt.Println(strOr(data.Name, "n/a"))
	},
}

var userEmailCmd = &cobra.Command{
	Use:   "email",
	Short: "Print the user's email address",
	Run: func(cmd *cobra.Command, args []string) {
		data := fetchProfile()
		if data == nil {
			return
		}
		fmt.Println(strOr(data.Email, "n/a"))
	},
}

func fetchProfile() *k8shelld.IdentityInfo {
	resp, err := client.MakeRequest("GET", "/profile", nil, nil)
	if err != nil {
		fmt.Println("Error fetching profile:", err)
		return nil
	}
	defer resp.Body.Close()

	if err := client.CheckApplicationError(resp); err != nil {
		fmt.Printf("%v\n", err)
		return nil
	}

	var data k8shelld.IdentityInfo
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		fmt.Printf("Error parsing response: %v\n", err)
		return nil
	}
	return &data
}

func init() {
	UserCmd.AddCommand(userNameCmd)
	UserCmd.AddCommand(userEmailCmd)
}
