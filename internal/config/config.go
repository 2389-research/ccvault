// ABOUTME: Configuration management for ccvault
// ABOUTME: Handles config file loading, defaults, and environment overrides

package config

import (
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

// Load reads configuration from file and environment
func Load() (*Config, error) {
	v := viper.New()

	// Set defaults
	v.SetDefault("claude_home", DefaultClaudeHome())
	v.SetDefault("data_dir", DefaultDataDir())

	// Environment variables
	v.SetEnvPrefix("CCVAULT")
	v.AutomaticEnv()

	// Config file
	v.SetConfigName("config")
	v.SetConfigType("toml")
	v.AddConfigPath(DefaultDataDir())
	v.AddConfigPath(".")

	// Read config file if it exists (not an error if missing)
	_ = v.ReadInConfig()

	return unmarshalAndApplyDefaults(v)
}

// LoadFrom reads configuration from a specific file path
func LoadFrom(configFile string) (*Config, error) {
	v := viper.New()

	// Set defaults
	v.SetDefault("claude_home", DefaultClaudeHome())
	v.SetDefault("data_dir", DefaultDataDir())

	v.SetConfigFile(configFile)

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	return unmarshalAndApplyDefaults(v)
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
