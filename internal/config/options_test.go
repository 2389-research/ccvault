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

// --- claude_home isolation (#68) ---

// Precedence for claude_home mirrors data_dir's: flag, then environment,
// then the config file, then the built-in default.
func TestLoadWith_ClaudeHomePrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := filepath.Join(home, "fixture")
	writeConfig(t, fixture, "claude_home = \"/from-config-file\"\n")

	t.Run("config file beats default", func(t *testing.T) {
		cfg, err := LoadWith(Options{DataDir: fixture})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.ClaudeHome != "/from-config-file" {
			t.Errorf("claude_home = %q, want the config-file value", cfg.ClaudeHome)
		}
	})

	t.Run("env beats config file", func(t *testing.T) {
		t.Setenv("CCVAULT_CLAUDE_HOME", "/from-env")
		cfg, err := LoadWith(Options{DataDir: fixture})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.ClaudeHome != "/from-env" {
			t.Errorf("claude_home = %q, want /from-env", cfg.ClaudeHome)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("CCVAULT_CLAUDE_HOME", "/from-env")
		cfg, err := LoadWith(Options{DataDir: fixture, ClaudeHome: "/from-flag"})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.ClaudeHome != "/from-flag" {
			t.Errorf("claude_home = %q, want /from-flag", cfg.ClaudeHome)
		}
	})

	t.Run("default when nothing set and nothing relocated", func(t *testing.T) {
		cfg, err := LoadWith(Options{})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.ClaudeHome != filepath.Join(home, ".claude") {
			t.Errorf("claude_home = %q, want the default under the swapped HOME", cfg.ClaudeHome)
		}
	})
}

// The flag has to reach the source list, not just the ClaudeHome field: sync
// reads cfg.Sources, so a [[sources]] entry left pointing at the real
// ~/.claude would scan it regardless of what claude_home says. This is the
// hole that made --data-dir's isolation incomplete.
func TestLoadWith_ClaudeHomeReplacesConfiguredSources(t *testing.T) {
	fixture := t.TempDir()
	scratch := t.TempDir()
	writeConfig(t, fixture, "[[sources]]\nname = \"claude-code\"\ntype = \"claude-code\"\npath = \"/real/claude/home\"\n\n[[sources]]\nname = \"codex\"\ntype = \"codex\"\npath = \"/real/codex/home\"\n")

	cfg, err := LoadWith(Options{DataDir: fixture, ClaudeHome: scratch})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}

	if len(cfg.Sources) != 1 {
		t.Fatalf("explicit claude home should leave exactly one source, got %d: %+v", len(cfg.Sources), cfg.Sources)
	}
	if cfg.Sources[0].Path != scratch {
		t.Errorf("source path = %q, want the scratch claude home %q", cfg.Sources[0].Path, scratch)
	}
	if cfg.Sources[0].Type != "claude-code" {
		t.Errorf("source type = %q, want claude-code", cfg.Sources[0].Type)
	}
}

// Same rule for the environment variable, which is the form a test harness
// or sandbox is most likely to use.
func TestLoadWith_EnvClaudeHomeReplacesConfiguredSources(t *testing.T) {
	fixture := t.TempDir()
	scratch := t.TempDir()
	writeConfig(t, fixture, "[[sources]]\nname = \"claude-code\"\ntype = \"claude-code\"\npath = \"/real/claude/home\"\n")
	t.Setenv("CCVAULT_CLAUDE_HOME", scratch)

	cfg, err := LoadWith(Options{DataDir: fixture})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].Path != scratch {
		t.Fatalf("CCVAULT_CLAUDE_HOME should replace the source list with the scratch tree, got %+v", cfg.Sources)
	}
}

// A config file that names BOTH claude_home and sources is hand-authored and
// internally consistent; the sources list it declares is kept. Only overrides
// from outside the file (flag, env) replace it.
func TestLoadWith_ConfigFileClaudeHomeKeepsItsOwnSources(t *testing.T) {
	fixture := t.TempDir()
	writeConfig(t, fixture, "claude_home = \"/claude-from-file\"\n\n[[sources]]\nname = \"a\"\ntype = \"claude-code\"\npath = \"/source-a\"\n\n[[sources]]\nname = \"b\"\ntype = \"codex\"\npath = \"/source-b\"\n")

	cfg, err := LoadWith(Options{DataDir: fixture})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("config-file sources should survive, got %+v", cfg.Sources)
	}
}

// The property that matters, stated as bluntly as possible: there is no way
// to pass --data-dir and end up reading the real ~/.claude by accident. It
// takes saying so.
func TestLoadWith_DataDirNeverSilentlyReachesTheRealClaudeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := filepath.Join(home, ".claude")

	for _, name := range []string{"no config file", "empty config file", "config file setting only data_dir"} {
		t.Run(name, func(t *testing.T) {
			fixture := t.TempDir()
			switch name {
			case "empty config file":
				writeConfig(t, fixture, "")
			case "config file setting only data_dir":
				writeConfig(t, fixture, "data_dir = \"/elsewhere\"\n")
			}

			cfg, err := LoadWith(Options{DataDir: fixture})
			if err != nil {
				t.Fatalf("LoadWith: %v", err)
			}
			if cfg.ClaudeHome == real {
				t.Errorf("claude_home resolved to the real %q", real)
			}
			for _, s := range cfg.Sources {
				if s.Path == real {
					t.Errorf("source %q points at the real %q", s.Name, real)
				}
			}
		})
	}
}

// --- --data-dir implies an isolated claude home (#68) ---

// The safety default. --data-dir is the ad-hoc, per-invocation form of
// relocating the archive — the one tests, agents and sandboxes type — and
// every agent on this repo was told to pass it to stay away from real data.
// So when it is given and the configuration says NOTHING about where to read
// conversations from, the source tree is inferred from the archive rather
// than left pointing at the user's real ~/.claude.
func TestLoadWith_DataDirImpliesAnIsolatedClaudeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fixture := t.TempDir()

	cfg, err := LoadWith(Options{DataDir: fixture})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}

	want := filepath.Join(fixture, "claude")
	if cfg.ClaudeHome != want {
		t.Errorf("claude_home = %q, want the inferred %q", cfg.ClaudeHome, want)
	}
	if cfg.ClaudeHome == filepath.Join(home, ".claude") {
		t.Error("--data-dir alone still resolved claude_home to the real ~/.claude")
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].Path != want {
		t.Fatalf("sources should follow the inferred claude home, got %+v", cfg.Sources)
	}
	if !cfg.Sources[0].Implied {
		t.Error("an inferred source must be marked Implied so sync can explain itself when the tree is absent")
	}
}

// The escape hatch for the case that motivated keeping the two flags
// independent: relocating the archive while still indexing the real source.
// Saying so explicitly — on the flag, in the environment, or in the config
// file — switches the implication off.
func TestLoadWith_SayingWhereToReadDisablesTheImplication(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("--claude-home", func(t *testing.T) {
		cfg, err := LoadWith(Options{DataDir: t.TempDir(), ClaudeHome: "/named/on/the/flag"})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.ClaudeHome != "/named/on/the/flag" {
			t.Errorf("claude_home = %q, want the flag value", cfg.ClaudeHome)
		}
		if cfg.Sources[0].Implied {
			t.Error("a claude home named on the flag is not implied")
		}
	})

	t.Run("CCVAULT_CLAUDE_HOME", func(t *testing.T) {
		t.Setenv("CCVAULT_CLAUDE_HOME", "/named/in/the/env")
		cfg, err := LoadWith(Options{DataDir: t.TempDir()})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.ClaudeHome != "/named/in/the/env" {
			t.Errorf("claude_home = %q, want the env value", cfg.ClaudeHome)
		}
		if cfg.Sources[0].Implied {
			t.Error("a claude home named in the environment is not implied")
		}
	})

	t.Run("claude_home in the config file", func(t *testing.T) {
		fixture := t.TempDir()
		writeConfig(t, fixture, "claude_home = \"/named/in/the/file\"\n")
		cfg, err := LoadWith(Options{DataDir: fixture})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.ClaudeHome != "/named/in/the/file" {
			t.Errorf("claude_home = %q, want the config-file value", cfg.ClaudeHome)
		}
		if cfg.Sources[0].Implied {
			t.Error("a claude home named in the config file is not implied")
		}
	})

	t.Run("a sources list in the config file", func(t *testing.T) {
		fixture := t.TempDir()
		writeConfig(t, fixture, "[[sources]]\nname = \"a\"\ntype = \"claude-code\"\npath = \"/declared/source\"\n")
		cfg, err := LoadWith(Options{DataDir: fixture})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if len(cfg.Sources) != 1 || cfg.Sources[0].Path != "/declared/source" {
			t.Fatalf("a declared sources list answers the question and must survive, got %+v", cfg.Sources)
		}
		if cfg.Sources[0].Implied {
			t.Error("a declared source is not implied")
		}
	})
}

// A durable relocation of the archive — config file or environment, not the
// ad-hoc flag — keeps the historical behaviour of reading the real source.
// That is the use the two independent flags were argued for, and it is
// untouched.
func TestLoadWith_DurableDataDirRelocationStillReadsTheRealSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("bare Load", func(t *testing.T) {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ClaudeHome != filepath.Join(home, ".claude") {
			t.Errorf("claude_home = %q, want the default", cfg.ClaudeHome)
		}
		if cfg.Sources[0].Implied {
			t.Error("the default source is not implied; nothing was relocated")
		}
	})

	t.Run("CCVAULT_DATA_DIR", func(t *testing.T) {
		t.Setenv("CCVAULT_DATA_DIR", filepath.Join(home, "env-archive"))
		cfg, err := LoadWith(Options{})
		if err != nil {
			t.Fatalf("LoadWith: %v", err)
		}
		if cfg.ClaudeHome != filepath.Join(home, ".claude") {
			t.Errorf("claude_home = %q, want the default; the env form is a durable relocation", cfg.ClaudeHome)
		}
		if cfg.Sources[0].Implied {
			t.Error("CCVAULT_DATA_DIR must not imply a source")
		}
	})
}
