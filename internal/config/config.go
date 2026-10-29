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

	// Implied marks a source nobody asked for: --data-dir was given, the
	// configuration said nothing about where to read conversations from, and
	// the path was inferred from the archive directory. Never read from a
	// config file — it is a fact about how this Config was resolved.
	//
	// Consumers use it to tell "the tree you named is missing" (the user's
	// problem) from "I guessed at a tree and guessed wrong" (ours, and worth
	// a different message). See internal/sync.
	Implied bool `mapstructure:"-"`
}

// ImpliedClaudeHomeDir is the subdirectory of the archive that --data-dir
// infers as its source tree when nothing says otherwise.
const ImpliedClaudeHomeDir = "claude"

// Config holds all ccvault configuration
type Config struct {
	ClaudeHome string         `mapstructure:"claude_home"`
	DataDir    string         `mapstructure:"data_dir"`
	Sources    []SourceConfig `mapstructure:"sources"`
	Schedule   ScheduleConfig `mapstructure:"schedule"`

	// ConfigFile is the file this Config was read from, or "" when no file
	// was found. Never read from a config file — it is a fact about how this
	// Config was resolved, like SourceConfig.Implied.
	//
	// Recorded because a setting that has to be written back has to go to the
	// file that was read, not to a guess at the default path. A run pointed at
	// a throwaway archive writes its own config file there and leaves the
	// real one alone.
	ConfigFile string `mapstructure:"-"`
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

// ClaudeHomeEnvVar is the environment variable that overrides claude_home.
// Named here because the override has to be detectable as *explicit* — an
// env-supplied claude home replaces the config file's source list, which
// viper's merged view can no longer tell apart from a file-supplied one.
const ClaudeHomeEnvVar = "CCVAULT_CLAUDE_HOME"

// Options names the configuration sources explicitly, so a caller can point
// ccvault at a throwaway archive AND a throwaway transcript tree without
// either default being consulted at all.
//
// They answer three different questions. ConfigFile is which file to read —
// which sources to scan, which claude_home, and possibly a data_dir of its
// own. DataDir is where the database lives, and it wins over whatever the
// file says. ClaudeHome is where conversations are read from, and it wins
// the same way.
//
// The two are not independent. DataDir is the ad-hoc, per-invocation way to
// relocate the archive — the form a test, an agent or a sandbox types — and
// passing it has always been understood to mean "stay away from my real
// data". So a DataDir given with nothing said about where to read
// conversations from infers its source tree from the archive instead of
// falling through to the user's real ~/.claude. Naming a source explicitly
// (ClaudeHome, CCVAULT_CLAUDE_HOME, claude_home or sources in the config
// file) switches that off, which is how the archive gets relocated while the
// real transcripts are still indexed. A durable relocation via
// CCVAULT_DATA_DIR or the config file's data_dir infers nothing.
type Options struct {
	// ConfigFile is the one file to read. When set, no directory is
	// searched and a missing file is an error rather than a fallback.
	ConfigFile string

	// DataDir overrides the data directory. It also REPLACES the config
	// search path, so the default data directory is not stat'd at all.
	DataDir string

	// ClaudeHome overrides the Claude Code transcript directory. It also
	// REPLACES any [[sources]] list the config file declares — see
	// applyExplicitClaudeHome for why a partial override would be a
	// foot-gun rather than a feature.
	ClaudeHome string
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
// claude_home follows the identical chain with opts.ClaudeHome (the
// --claude-home flag) and CCVAULT_CLAUDE_HOME.
//
// Rung 1 is viper's explicit-Set layer, which sits above AutomaticEnv; the
// rest is viper's own ordering. All four rungs are pinned by tests rather
// than assumed.
func LoadWith(opts Options) (*Config, error) {
	v := viper.New()

	// Set defaults
	v.SetDefault("claude_home", DefaultClaudeHome())
	v.SetDefault("data_dir", DefaultDataDir())
	// On by default: the notice is silent unless something is actually wrong,
	// and someone who has never run `ccvault schedule` is exactly who it is
	// for. notify = false turns it off.
	v.SetDefault(ScheduleNotifyKey, true)

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

	// Applied after the file is read so they outrank the file's own values
	// as well as the environment.
	if opts.DataDir != "" {
		v.Set("data_dir", opts.DataDir)
	}

	// Decided before claude_home is overridden, because the question is
	// whether ANYTHING answered "where do I read conversations from" —
	// and after that Set the answer would always look like yes.
	implied := impliedClaudeHome(v, opts)
	if implied != "" {
		v.Set("claude_home", implied)
	}
	if opts.ClaudeHome != "" {
		v.Set("claude_home", opts.ClaudeHome)
	}

	cfg, err := unmarshalAndApplyDefaults(v)
	if err != nil {
		return nil, err
	}
	cfg.ConfigFile = v.ConfigFileUsed()

	// Done after defaults so it overwrites a synthesized source as well as a
	// declared one.
	switch {
	case implied != "":
		applyExplicitClaudeHome(cfg)
		cfg.Sources[0].Implied = true
	case opts.ClaudeHome != "" || os.Getenv(ClaudeHomeEnvVar) != "":
		applyExplicitClaudeHome(cfg)
	}

	return cfg, nil
}

// impliedClaudeHome returns the source tree to infer from the archive
// directory, or "" when the configuration already answers where conversations
// come from.
//
// This is the safety default behind #68. --data-dir exists so a test or an
// agent can drive ccvault against a scratch archive, and everyone who passes
// it believes that means isolated — but claude_home used to fall through to
// the real ~/.claude, so a `sync` read every transcript on the machine. An
// opt-in flag alone would only protect the people who remember it, leaving
// the trap armed for anyone who passes one flag and not the other.
//
// The inference is deliberately narrow. It needs the --data-dir FLAG, not
// CCVAULT_DATA_DIR or a config file's data_dir: the flag is the ad-hoc form a
// sandbox types, while the other two are durable relocations whose owners
// mean to keep indexing the real source. And it needs the configuration to
// have said nothing at all — neither claude_home nor a sources list, in the
// file or the environment — because any of those IS the answer to the
// question being inferred.
func impliedClaudeHome(v *viper.Viper, opts Options) string {
	switch {
	case opts.DataDir == "":
		return ""
	case opts.ClaudeHome != "", os.Getenv(ClaudeHomeEnvVar) != "":
		return ""
	case v.InConfig("claude_home"), v.InConfig("sources"):
		return ""
	}
	return filepath.Join(opts.DataDir, ImpliedClaudeHomeDir)
}

// applyExplicitClaudeHome collapses the source list to the single claude-code
// source rooted at cfg.ClaudeHome.
//
// The point of naming a claude home from outside the config file is that
// nothing reads the real ~/.claude. A [[sources]] entry with an explicit path
// would survive a claude_home-only override and scan it anyway, which is the
// isolation hole --claude-home exists to close — so the override is total
// rather than partial. A config file that declares both claude_home and
// sources is hand-authored and internally consistent, so it keeps its list;
// only the flag and the environment variable replace it.
func applyExplicitClaudeHome(cfg *Config) {
	cfg.Sources = []SourceConfig{{
		Name: "claude-code",
		Type: "claude-code",
		Path: cfg.ClaudeHome,
	}}
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
