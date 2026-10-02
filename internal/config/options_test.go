// ABOUTME: Tests for explicit config-file / data-dir overrides
// ABOUTME: Pins the precedence order and proves the default data dir leaves the config search path when overridden

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a TOML config file and returns its path.
func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestConfigSearchPaths_DefaultIncludesDefaultDataDir(t *testing.T) {
	paths := configSearchPaths(Options{})

	if len(paths) == 0 || paths[0] != DefaultDataDir() {
		t.Fatalf("default search path should start at %s, got %v", DefaultDataDir(), paths)
	}
}

// The whole point of the override: an explicit data dir REPLACES the search
// path instead of being prepended to it, so the real ~/.ccvault is never
// consulted — not even stat'd — when a throwaway dir is named.
func TestConfigSearchPaths_DataDirOverrideReplacesDefault(t *testing.T) {
	tmp := t.TempDir()

	paths := configSearchPaths(Options{DataDir: tmp})

	if len(paths) != 1 || paths[0] != tmp {
		t.Fatalf("override should yield exactly [%s], got %v", tmp, paths)
	}
	for _, p := range paths {
		if p == DefaultDataDir() {
			t.Fatalf("default data dir %s still in search path %v", DefaultDataDir(), paths)
		}
	}
}

// An explicit config file names the only file to read, so there is no
// directory search at all.
func TestConfigSearchPaths_ConfigFileOverrideSearchesNothing(t *testing.T) {
	paths := configSearchPaths(Options{ConfigFile: "/somewhere/ccvault.toml"})

	if len(paths) != 0 {
		t.Fatalf("explicit config file should search no directories, got %v", paths)
	}
}

// Precedence rung 1: an explicit data dir beats CCVAULT_DATA_DIR, which
// beats the config file value, which beats the built-in default.
func TestLoadWith_DataDirPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := filepath.Join(home, "fixture")
	writeConfig(t, fixture, "data_dir = \"/from-config-file\"\nclaude_home = \"/from-config-file/claude\"\n")

	t.Run("config file beats default", func(t *testing.T) {
		cfg, err := LoadWith(Options{DataDir: fixture})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		// DataDir itself is overridden by the flag, so claude_home is what
		// shows the file was actually read.
		if cfg.ClaudeHome != "/from-config-file/claude" {
			t.Errorf("claude_home = %q, want the config-file value", cfg.ClaudeHome)
		}
	})

	t.Run("env beats config file", func(t *testing.T) {
		t.Setenv("CCVAULT_DATA_DIR", "/from-env")
		cfg, err := LoadWith(Options{ConfigFile: filepath.Join(fixture, "config.toml")})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.DataDir != "/from-env" {
			t.Errorf("data_dir = %q, want /from-env (env must beat the config file)", cfg.DataDir)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("CCVAULT_DATA_DIR", "/from-env")
		cfg, err := LoadWith(Options{DataDir: "/from-flag"})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.DataDir != "/from-flag" {
			t.Errorf("data_dir = %q, want /from-flag (flag must beat env)", cfg.DataDir)
		}
	})

	t.Run("default when nothing set", func(t *testing.T) {
		cfg, err := LoadWith(Options{})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.DataDir != filepath.Join(home, ".ccvault") {
			t.Errorf("data_dir = %q, want the default under the swapped HOME", cfg.DataDir)
		}
	})
}

// Behavioural counterpart to TestConfigSearchPaths_DataDirOverrideReplacesDefault:
// a config.toml sitting in the default data dir is read by a bare Load() and
// must be invisible once an override is given. HOME is swapped so this
// exercises a throwaway default data dir, never the real one.
func TestLoadWith_OverrideIgnoresConfigInDefaultDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeConfig(t, filepath.Join(home, ".ccvault"), "claude_home = \"/default-data-dir-was-read\"\n")

	bare, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if bare.ClaudeHome != "/default-data-dir-was-read" {
		t.Fatalf("precondition failed: Load() did not read $HOME/.ccvault/config.toml (claude_home = %q)", bare.ClaudeHome)
	}

	throwaway := filepath.Join(home, "throwaway")
	overridden, err := LoadWith(Options{DataDir: throwaway})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if overridden.ClaudeHome == "/default-data-dir-was-read" {
		t.Error("--data-dir override still read the config file in the default data dir")
	}
	if overridden.DataDir != throwaway {
		t.Errorf("data_dir = %q, want %q", overridden.DataDir, throwaway)
	}
}

// A config file named explicitly but absent is a typo, not a fallback.
func TestLoadWith_MissingExplicitConfigFileErrors(t *testing.T) {
	_, err := LoadWith(Options{ConfigFile: filepath.Join(t.TempDir(), "absent.toml")})
	if err == nil {
		t.Fatal("expected an error for a missing explicit config file")
	}
}

// A config.toml absent from an explicit data dir is the normal case, not an error.
func TestLoadWith_MissingConfigInDataDirIsFine(t *testing.T) {
	tmp := t.TempDir()

	cfg, err := LoadWith(Options{DataDir: tmp})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if cfg.DataDir != tmp {
		t.Errorf("data_dir = %q, want %q", cfg.DataDir, tmp)
	}
}

// A config file that is present but unparseable must not be swallowed. If it
// were, a fixture run would fall back to defaults and quietly scan the real
// ~/.claude instead of the fixture's source tree.
func TestLoadWith_MalformedConfigInDataDirErrors(t *testing.T) {
	tmp := t.TempDir()
	writeConfig(t, tmp, "claude_home = \"unterminated\n[[[\n")

	_, err := LoadWith(Options{DataDir: tmp})
	if err == nil {
		t.Fatal("expected an error for a malformed config file, got nil")
	}
	if !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("error should name the offending file, got: %v", err)
	}
}

// Both flags together: the file names where settings come from, the data dir
// flag still wins for data_dir itself.
func TestLoadWith_DataDirBeatsExplicitConfigFile(t *testing.T) {
	fixture := t.TempDir()
	path := writeConfig(t, fixture, "data_dir = \"/from-config-file\"\nclaude_home = \"/claude-from-file\"\n")

	cfg, err := LoadWith(Options{ConfigFile: path, DataDir: "/from-flag"})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if cfg.DataDir != "/from-flag" {
		t.Errorf("data_dir = %q, want /from-flag", cfg.DataDir)
	}
	if cfg.ClaudeHome != "/claude-from-file" {
		t.Errorf("claude_home = %q, want /claude-from-file", cfg.ClaudeHome)
	}
	if !strings.HasPrefix(cfg.Sources[0].Path, "/claude-from-file") {
		t.Errorf("backward-compat source path = %q, want it derived from claude_home", cfg.Sources[0].Path)
	}
}
