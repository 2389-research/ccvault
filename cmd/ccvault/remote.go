// ABOUTME: ccvault remote add/list/remove — manages configured remote vaults
// ABOUTME: Writes to ~/.ccvault/config.toml via a small TOML shim

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/2389-research/ccvault/internal/config"
)

var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Manage configured remote vaults",
}

var remoteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured remotes",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if len(cfg.Remotes) == 0 {
			fmt.Println("No remotes configured.")
			return nil
		}
		for name, r := range cfg.Remotes {
			fmt.Printf("%-20s %s\n", name, r.URL)
		}
		return nil
	},
}

var remoteAddCmd = &cobra.Command{
	Use:   "add [name] [url]",
	Short: "Add a remote vault",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return writeRemote(args[0], args[1])
	},
}

var remoteRemoveCmd = &cobra.Command{
	Use:   "remove [name]",
	Short: "Remove a remote vault",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return deleteRemote(args[0])
	},
}

func writeRemote(name, url string) error {
	cfg, err := loadRawConfigTOML()
	if err != nil {
		return err
	}
	if cfg["remotes"] == nil {
		cfg["remotes"] = map[string]any{}
	}
	remotes, ok := cfg["remotes"].(map[string]any)
	if !ok {
		return fmt.Errorf("existing [remotes] entry is not a table")
	}
	remotes[name] = map[string]any{"url": url}
	return writeRawConfigTOML(cfg)
}

func deleteRemote(name string) error {
	cfg, err := loadRawConfigTOML()
	if err != nil {
		return err
	}
	if remotes, ok := cfg["remotes"].(map[string]any); ok {
		delete(remotes, name)
	}
	return writeRawConfigTOML(cfg)
}

func configPath() string {
	return filepath.Join(config.DefaultDataDir(), "config.toml")
}

func loadRawConfigTOML() (map[string]any, error) {
	data, err := os.ReadFile(configPath())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	out := map[string]any{}
	if len(data) > 0 {
		if err := toml.Unmarshal(data, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func writeRawConfigTOML(cfg map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0755); err != nil {
		return err
	}
	b, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), b, 0644)
}
