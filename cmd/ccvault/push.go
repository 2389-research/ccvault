// ABOUTME: ccvault push [remote] — push local sessions to a configured remote
// ABOUTME: Wires config + DB + remote client together; incremental by default

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/remote/client"
)

var pushCmd = &cobra.Command{
	Use:   "push [remote]",
	Short: "Push local sessions to a remote vault",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		cfg, err := config.Load()
		if err != nil {
			return err
		}

		remoteName := "origin"
		if len(args) == 1 {
			remoteName = args[0]
		}
		r, ok := cfg.Remotes[remoteName]
		if !ok {
			return fmt.Errorf("remote %q not configured; run `ccvault remote add %s <url>`", remoteName, remoteName)
		}

		cli, err := client.FromRemoteURL(r.URL)
		if err != nil {
			return fmt.Errorf("build client: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return err
		}
		defer func() { _ = database.Close() }()

		stats, err := client.Push(cli, database, remoteName, dryRun)
		if err != nil {
			return err
		}

		if dryRun {
			fmt.Printf("[dry-run] would push %d sessions to %s\n", stats.SessionsPushed, remoteName)
		} else {
			fmt.Printf("Pushed %d sessions (%d turns) to %s\n",
				stats.SessionsPushed, stats.TurnsPushed, remoteName)
		}
		return nil
	},
}

func init() {
	pushCmd.Flags().Bool("dry-run", false, "Print what would be pushed without sending")
}
