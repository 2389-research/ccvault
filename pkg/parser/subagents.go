// ABOUTME: Discovery of subagent (sidechain) transcripts under a .claude/projects tree.
// ABOUTME: Separate from ScanClaudeHome because the two have different naming rules.

package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// subagentDirName is the directory Claude Code writes sidechain
	// transcripts into, one level below the parent session's uuid directory.
	subagentDirName = "subagents"

	// subagentFilePrefix starts every sidechain transcript's filename. The
	// rest is the agentId, which is hex and not a uuid — which is why
	// ScanClaudeHome's isValidUUID check rejects these files and they need
	// their own walk.
	subagentFilePrefix = "agent-"
)

// ScanSubagentFiles walks a .claude/projects tree and returns every subagent
// transcript in it:
//
//	<projectsDir>/<encoded-project>/<parent-uuid>/subagents/agent-<hex>.jsonl
//
// SessionID is the filename stem ("agent-<hex>") rather than a session uuid.
// The transcript's in-band sessionId field holds the PARENT's uuid, so it is
// not usable as an identity — adapters mint a composite id instead (see
// adapter.SubagentSessionID). ProjectPath is the lossy decode of the encoded
// project directory, the same best-effort value ScanClaudeHome produces; the
// cwd inside the transcript remains the authoritative source.
//
// A missing tree is not an error — a source with no sessions yet has no
// subagents either.
func ScanSubagentFiles(projectsDir string) ([]SessionFile, error) {
	info, err := os.Stat(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat projects dir %s: %w", projectsDir, err)
	}
	if !info.IsDir() {
		return nil, nil
	}

	var files []SessionFile
	walkErr := filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) != subagentDirName {
			return nil
		}
		name := d.Name()
		// Excludes the .meta.json sibling that sits next to every
		// transcript, and anything else that happens to land in the dir.
		if !strings.HasPrefix(name, subagentFilePrefix) || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}

		sf := SessionFile{
			Path:        path,
			SessionID:   strings.TrimSuffix(name, ".jsonl"),
			ProjectPath: subagentProjectPath(projectsDir, path),
		}
		if fi, err := d.Info(); err == nil {
			sf.ModTime = fi.ModTime()
		}
		files = append(files, sf)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk subagents: %w", walkErr)
	}

	return files, nil
}

// subagentProjectPath decodes the project directory a subagent transcript
// lives under. The encoded project name is the FIRST path segment below
// projectsDir; decoding the whole relative path would fold the parent-uuid and
// "subagents" segments into the result.
func subagentProjectPath(projectsDir, path string) string {
	rel, err := filepath.Rel(projectsDir, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	return decodeProjectPath(parts[0])
}
