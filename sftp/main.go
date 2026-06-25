// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Command sftp is a wrapper around the sftp server that allows it to be used
// as a separate process by k8shelld.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/pkg/sftp"
)

func main() {
	var options []sftp.ServerOption

	workingDir := os.Getenv("HOME")
	if workingDir == "" {
		workingDir = "/"
	}

	options = append(options, sftp.WithServerWorkingDirectory(workingDir))

	svr, _ := sftp.NewServer(
		struct {
			io.Reader
			io.WriteCloser
		}{os.Stdin,
			os.Stdout,
		},
		options...,
	)
	if err := svr.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "sftp server completed with error: %v", err)
		os.Exit(1)
	}
}
