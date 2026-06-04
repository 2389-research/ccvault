// ABOUTME: Tests for the config package
// ABOUTME: Verifies configuration loading, default values, and multi-source support

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultClaudeHome(t *testing.T) {
	home := DefaultClaudeHome()

	if home == "" {
		t.Error("DefaultClaudeHome returned empty string")
	}

	// Should end with .claude
	if filepath.Base(home) != ".claude" {
		t.Errorf("DefaultClaudeHome should end with .claude, got %s", home)
	}

	// Should be under home directory
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("Failed to get user home dir: %v", err)
	}
	expected := filepath.Join(userHome, ".claude")
	if home != expected {
		t.Errorf("Expected %s, got %s", expected, home)
	}
}

func TestDefaultDataDir(t *testing.T) {
	dataDir := DefaultDataDir()

	if dataDir == "" {
		t.Error("DefaultDataDir returned empty string")
	}

	// Should end with .ccvault
	if filepath.Base(dataDir) != ".ccvault" {
		t.Errorf("DefaultDataDir should end with .ccvault, got %s", dataDir)
	}

	// Should be under home directory
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("Failed to get user home dir: %v", err)
	}
	expected := filepath.Join(userHome, ".ccvault")
	if dataDir != expected {
		t.Errorf("Expected %s, got %s", expected, dataDir)
	}
}

func TestLoad(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg == nil {
		t.Fatal("Load returned nil config")
	}

	// Should have default values
	if cfg.ClaudeHome == "" {
		t.Error("ClaudeHome is empty")
	}
	if cfg.DataDir == "" {
		t.Error("DataDir is empty")
	}
}

func TestLoad_MultiSource(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	content := `
sources:
  - name: "claude-code"
    type: "claude-code"
    path: "/home/user/.claude"
  - name: "aider"
    type: "aider"
    path: "/home/user/.aider"
`
	if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadFrom(configFile)
	if err != nil {
		t.Fatalf("LoadFrom failed: %v", err)
	}

	if len(cfg.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(cfg.Sources))
	}

	// Verify first source
	if cfg.Sources[0].Name != "claude-code" {
		t.Errorf("source[0].Name = %q, want %q", cfg.Sources[0].Name, "claude-code")
	}
	if cfg.Sources[0].Type != "claude-code" {
		t.Errorf("source[0].Type = %q, want %q", cfg.Sources[0].Type, "claude-code")
	}
	if cfg.Sources[0].Path != "/home/user/.claude" {
		t.Errorf("source[0].Path = %q, want %q", cfg.Sources[0].Path, "/home/user/.claude")
	}

	// Verify second source
	if cfg.Sources[1].Name != "aider" {
		t.Errorf("source[1].Name = %q, want %q", cfg.Sources[1].Name, "aider")
	}
	if cfg.Sources[1].Type != "aider" {
		t.Errorf("source[1].Type = %q, want %q", cfg.Sources[1].Type, "aider")
	}
	if cfg.Sources[1].Path != "/home/user/.aider" {
		t.Errorf("source[1].Path = %q, want %q", cfg.Sources[1].Path, "/home/user/.aider")
	}
}

func TestLoad_BackwardCompat(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	content := `claude_home: "/home/user/.claude"
`
	if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadFrom(configFile)
	if err != nil {
		t.Fatalf("LoadFrom failed: %v", err)
	}

	// Should auto-create a single claude-code source
	if len(cfg.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(cfg.Sources))
	}

	if cfg.Sources[0].Name != "claude-code" {
		t.Errorf("source.Name = %q, want %q", cfg.Sources[0].Name, "claude-code")
	}
	if cfg.Sources[0].Type != "claude-code" {
		t.Errorf("source.Type = %q, want %q", cfg.Sources[0].Type, "claude-code")
	}
	if cfg.Sources[0].Path != "/home/user/.claude" {
		t.Errorf("source.Path = %q, want %q", cfg.Sources[0].Path, "/home/user/.claude")
	}

	// ClaudeHome should still be set for backward compat
	if cfg.ClaudeHome != "/home/user/.claude" {
		t.Errorf("ClaudeHome = %q, want %q", cfg.ClaudeHome, "/home/user/.claude")
	}
}

func TestLoad_DuplicateSourceNames(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	content := `
sources:
  - name: "primary"
    type: "claude-code"
    path: "/home/user/.claude"
  - name: "primary"
    type: "codex"
    path: "/home/user/.codex"
`
	if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	if _, err := LoadFrom(configFile); err == nil {
		t.Fatal("expected error for duplicate source names, got nil")
	}
}

func TestLoad_EmptySourceName(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	content := `
sources:
  - name: ""
    type: "claude-code"
    path: "/home/user/.claude"
`
	if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	if _, err := LoadFrom(configFile); err == nil {
		t.Fatal("expected error for empty source name, got nil")
	}
}

func TestEnsureDataDir(t *testing.T) {
	// Create a temp directory for testing
	tmpDir := t.TempDir()
	testDataDir := filepath.Join(tmpDir, "test-ccvault")

	cfg := &Config{
		DataDir: testDataDir,
	}

	err := EnsureDataDir(cfg)
	if err != nil {
		t.Fatalf("EnsureDataDir failed: %v", err)
	}

	// Verify directory was created
	info, err := os.Stat(testDataDir)
	if err != nil {
		t.Fatalf("Failed to stat created directory: %v", err)
	}
	if !info.IsDir() {
		t.Error("EnsureDataDir did not create a directory")
	}
}
