package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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
	ValidateCmd.Flags().BoolVarP(&compose, "compose", "c", false, "Test to compose the final blueprint")
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

		cwd, err := os.Getwd()
		if err != nil {
			fmt.Printf("Error getting current working directory: %v\n", err)
			os.Exit(1)
		}

		if _, err := os.Stat(filepath.Join(cwd, k8shellFile)); os.IsNotExist(err) {
			fmt.Printf("Error: file '%s' does not exist\n", k8shellFile)
			os.Exit(1)
		}

		absPath, err := filepath.Abs(filepath.Join(cwd, k8shellFile))
		if err != nil {
			fmt.Printf("Error getting absolute path: %v\n", err)
			os.Exit(1)
		}

		resp, err := client.MakeRequest("POST", fmt.Sprintf("/validate?file=%s&compose=%t", absPath, compose), nil, nil)
		if err != nil {
			fmt.Printf("Error validating k8shell file: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
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
