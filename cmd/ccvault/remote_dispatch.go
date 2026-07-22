// ABOUTME: Small helpers to route a command over SSH to a remote when --remote is set
// ABOUTME: Keeps the per-command Cobra code short and consistent

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/remote/client"
)

// runOnRemote dials the named remote from config, executes the command,
// and copies its stdout to os.Stdout. Returns (ranRemote, error).
// If ranRemote is false, the caller should fall through to local execution.
func runOnRemote(cmd *cobra.Command, command string) (bool, error) {
	name, _ := cmd.Flags().GetString("remote")
	if name == "" {
		return false, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return true, err
	}
	r, ok := cfg.Remotes[name]
	if !ok {
		return true, fmt.Errorf("remote %q not configured", name)
	}
	cli, err := client.FromRemoteURL(r.URL)
	if err != nil {
		return true, err
	}
	reader, _, err := cli.Run(command, nil)
	if err != nil {
		return true, err
	}
	defer func() { _ = reader.Close() }()
	br := bufio.NewReader(reader)
	_, err = io.Copy(os.Stdout, br)
	return true, err
}
