package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/spf13/cobra"
)

var (
	k8shellFile string
	compose     bool
)

func init() {
	ValidateCmd.Flags().StringVarP(&k8shellFile, "file", "f", "", "Path to the k8shell file")
	ValidateCmd.MarkFlagRequired("file")
	ValidateCmd.Flags().BoolVarP(&compose, "compose", "c", true, "Test to compose the final blueprint")
}

var ValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate k8shell file",
	Long:  `Validate the syntax and structure of a k8shell file.`,

	Run: func(cmd *cobra.Command, args []string) {
		if k8shellFile == "" {
			fmt.Println("Error: file path is required")
			os.Exit(1)
		}

		resp, err := client.MakeRequest("POST", fmt.Sprintf("/validate?file=%s&compose=%t",
			k8shellFile, compose), nil, nil)
		if err != nil {
			fmt.Printf("Error validating k8shell file: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			fmt.Printf("Validation failed with status: %s\n", resp.Status)
			os.Exit(1)
		}

		var response models.K8shellFileValidationResponse
		decoder := json.NewDecoder(resp.Body)
		if err := decoder.Decode(&response); err != nil {
			fmt.Printf("Error decoding response: %v\n", err)
			os.Exit(1)
		}

		if response.Status == "valid" {
			fmt.Printf("k8shell file %s is valid.\n", response.Filename)
			return
		}

		fmt.Printf("k8shell file %s is invalid.\n", response.Filename)
		for _, err := range response.Errors {
			fmt.Printf(" - %s\n", err)
		}

		os.Exit(1)
	},
}
