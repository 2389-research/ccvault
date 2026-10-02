// ABOUTME: Configuration management for ccvault
// ABOUTME: Handles config file loading, defaults, and environment overrides

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// SourceConfig describes a single conversation source (e.g. claude-code, aider)
type SourceConfig struct {
	Name string `mapstructure:"name"`
	Type string `mapstructure:"type"`
	Path string `mapstructure:"path"`
}

// Config holds all ccvault configuration
type Config struct {
	ClaudeHome string         `mapstructure:"claude_home"`
	DataDir    string         `mapstructure:"data_dir"`
	Sources    []SourceConfig `mapstructure:"sources"`
}

// DefaultClaudeHome returns the default Claude Code data directory
func DefaultClaudeHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// DefaultDataDir returns the default ccvault data directory
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ccvault")
}

// Options names the configuration sources explicitly, so a caller can point
// ccvault at a throwaway archive without the default data directory being
// consulted at all.
//
// They answer two different questions. ConfigFile is which file to read —
// which sources to scan, which claude_home, and possibly a data_dir of its
// own. DataDir is where the database lives, and it wins over whatever the
// file says.
type Options struct {
	// ConfigFile is the one file to read. When set, no directory is
	// searched and a missing file is an error rather than a fallback.
	ConfigFile string

	// DataDir overrides the data directory. It also REPLACES the config
	// search path, so the default data directory is not stat'd at all.
	DataDir string
}

// Load reads configuration from the default search path and environment.
func Load() (*Config, error) {
	return LoadWith(Options{})
}

// LoadFrom reads configuration from a specific file path.
func LoadFrom(configFile string) (*Config, error) {
	return LoadWith(Options{ConfigFile: configFile})
}

// LoadWith reads configuration honouring explicit overrides.
//
// Precedence for data_dir, highest first:
//
//  1. opts.DataDir (the --data-dir flag)
//  2. CCVAULT_DATA_DIR
//  3. data_dir in the config file
//  4. DefaultDataDir()
//
// Rung 1 is viper's explicit-Set layer, which sits above AutomaticEnv; the
// rest is viper's own ordering. All four rungs are pinned by tests rather
// than assumed.
func LoadWith(opts Options) (*Config, error) {
	v := viper.New()

	// Set defaults
	v.SetDefault("claude_home", DefaultClaudeHome())
	v.SetDefault("data_dir", DefaultDataDir())

	// Environment variables
	v.SetEnvPrefix("CCVAULT")
	v.AutomaticEnv()

	// Config file
	if opts.ConfigFile != "" {
		v.SetConfigFile(opts.ConfigFile)
		// An explicitly named file that isn't there is a typo, not a
		// reason to silently fall back to defaults.
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", opts.ConfigFile, err)
		}
	} else {
		v.SetConfigName("config")
		v.SetConfigType("toml")
		for _, dir := range configSearchPaths(opts) {
			v.AddConfigPath(dir)
		}
		// No config file on the search path is the normal case. One that is
		// there and cannot be parsed is not: falling back to defaults would
		// silently point claude_home at the real ~/.claude, which for a
		// fixture run is the opposite of what was asked for.
		// Viper's parse errors do not name the file, so wrap with the path
		// it settled on — otherwise the user has to guess which of the
		// search-path directories the broken file is in.
		var notFound viper.ConfigFileNotFoundError
		if err := v.ReadInConfig(); err != nil && !errors.As(err, &notFound) {
			return nil, fmt.Errorf("read config %s: %w", v.ConfigFileUsed(), err)
		}
	}

	// Applied after the file is read so it outranks the file's own data_dir
	// as well as the environment.
	if opts.DataDir != "" {
		v.Set("data_dir", opts.DataDir)
	}

	return unmarshalAndApplyDefaults(v)
}

// configSearchPaths returns the directories to search for config.toml.
//
// An explicit ConfigFile searches nothing. An explicit DataDir replaces the
// default list rather than extending it — the point of the override is that
// the default data directory is never touched, so prepending would defeat
// it. With neither, the historical list applies: the default data directory,
// then the working directory.
func configSearchPaths(opts Options) []string {
	switch {
	case opts.ConfigFile != "":
		return nil
	case opts.DataDir != "":
		return []string{opts.DataDir}
	default:
		return []string{DefaultDataDir(), "."}
	}
}

// unmarshalAndApplyDefaults decodes config and applies backward-compat defaults
func unmarshalAndApplyDefaults(v *viper.Viper) (*Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// Backward compat: if no sources configured, create one from ClaudeHome
	if len(cfg.Sources) == 0 {
		cfg.Sources = []SourceConfig{{
			Name: "claude-code",
			Type: "claude-code",
			Path: cfg.ClaudeHome,
		}}
	}

	if err := validateSources(cfg.Sources); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validateSources rejects configurations with duplicate or empty source names,
// since the --source filter and other lookups key off Name.
func validateSources(sources []SourceConfig) error {
	seen := make(map[string]int, len(sources))
	for i, s := range sources {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("sources[%d]: name is required", i)
		}
		if prev, ok := seen[s.Name]; ok {
			return fmt.Errorf("duplicate source name %q at sources[%d] and sources[%d]; names must be unique", s.Name, prev, i)
		}
		seen[s.Name] = i
	}
	return nil
}

// EnsureDataDir creates the data directory if it doesn't exist
func EnsureDataDir(cfg *Config) error {
	return os.MkdirAll(cfg.DataDir, 0o750)
}
