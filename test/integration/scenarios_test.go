// ABOUTME: Guard test that keeps scenarios.jsonl mapped to actual test coverage
// ABOUTME: Fails when a scenario has no verification entry or an entry goes stale

package integration

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// scenarioCoverage maps each scenario name in scenarios.jsonl to where it
// is verified. Adding a scenario without a coverage entry fails this test;
// so does removing a scenario and leaving its entry behind.
var scenarioCoverage = map[string]string{
	"sync-real-claude-code":       "internal/sync TestRunWithClaudeCodeAdapter; TestMultiSourceSyncAndSearch (claude-code fixture)",
	"sync-real-codex":             "TestMultiSourceSyncAndSearch (codex fixture)",
	"multi-source-sync":           "TestMultiSourceSyncAndSearch; internal/sync TestMultipleSourcesSync",
	"source-filtered-search":      "TestMultiSourceSyncAndSearch (source:codex / source:claude-code assertions)",
	"incremental-sync":            "internal/sync TestIncrementalSyncSkipsUnchanged + TestNeedsSyncWithMtimeMap",
	"migration-bootstrap":         "internal/db TestProjectSourceDefaultsToClaudeCode + TestSessionSourceDefaultsToClaudeCode",
	"adapter-registry":            "pkg/adapter registry_test.go",
	"backward-compat-config":      "internal/config config_test.go",
	"binary-builds":               "make build (Makefile target); go test ./... compiles all packages",
	"sync-real-jeff":              "TestMultiSourceSyncAndSearch (jeff fixture)",
	"sync-real-hex":               "TestMultiSourceSyncAndSearch (hex fixture)",
	"source-filtered-search-jeff": "TestMultiSourceSyncAndSearch (source:jeff assertions)",
	"fts-across-sources":          "TestMultiSourceSyncAndSearch (unfiltered 'hello' reaches all four sources)",
	// issue #11 (PR #12): oversized JSONL lines + truncation + malformed handling
	"oversized-jsonl-line-does-not-drop-session": "pkg/parser TestParseSessionReader_OversizedLineDoesNotDropSession + TestReadLineBounded_OversizedMiddleLineIsSkippedAndDrained",
	"malformed-only-file-surfaces-skipped-lines": "pkg/parser TestParseSessionReader_MalformedLinesAreSkippedAndCounted",
	"raw-json-truncation-persists-to-db":         "pkg/parser TestParseSessionReaderWithLimits_TruncatesLargeRawJSON + TestStrippedRawJSON_ShapeAndFields",
	"base64-payload-does-not-pollute-fts":        "pkg/parser TestExtractUserContent_IgnoresImageBlocks",
	// PR #10: agent surface hardening
	"tool-filter-case-insensitive":   "internal/search TestSearch_ToolFilterIsCaseInsensitive",
	"empty-tool-filter-returns-hint": "internal/mcp TestSearchConversations_EmptyResultsIncludeHint",
	"list-sessions-pagination-hint":  "internal/mcp TestListSessions_ReportsHasMore",
	"orient-warns-on-stat-failure":   "cmd/ccvault TestGatherOrientation_CollectsWarningsOnFailure",
	"analytics-unavailable-hint":     "internal/mcp TestGetAnalytics_ReportsUnavailableAnalytics",
	// PR #22 (beau): --full sync fix + display improvements
	"full-sync-clears-stale-data":             "internal/db TestResetAll_ClearsDataAndPreservesSchema + internal/sync TestSyncer_FullFlagClearsStaleData",
	"incremental-sync-preserves-stale-rows":   "internal/sync TestSyncer_IncrementalDoesNotClear",
	"display-name-is-basename":                "pkg/parser TestGetDisplayName",
	"tui-projects-shows-path-column":          "internal/tui TestProjectsModel_ViewShowsPathColumn",
	"tui-sessions-conditional-project-column": "internal/tui TestSessionsModel_ViewShowsProjectColumnWhenUnfiltered + TestSessionsModel_ViewOmitsProjectColumnWhenFiltered",
	"tui-search-vim-nav-respects-focus":       "internal/tui TestSearchModel_VimNavIgnoredWhileFocused",
	// PR #22 follow-ups 4-8: projectref discipline
	"projectref-class-a-tabular":        "internal/projectref TestLabel_* + internal/tui TestProjectsModel_ViewShowsPathColumn",
	"projectref-class-b-inline":         "internal/projectref TestInline_*",
	"projectref-class-c-structured":     "internal/projectref TestRef_* + TestRefsFromValues_PreservesOrder",
	"projectref-class-d-input-matching": "internal/projectref TestResolveAll_*",
	"projectref-ast-allowlist-enforced": "test/integration TestProjectRefEnforcement",
	"list-projects-sort-tiebreaker":     "internal/db TestGetProjects_SortStableTiebreaker",
	// PR #22 follow-up 8: width-aware compaction
	"compact-path-progressive-initialing":      "internal/compact TestPath_*",
	"compact-model-never-drops-datestamp":      "internal/compact TestModel_NeverDropsTrailingDatestamp",
	"compact-source-ladder":                    "internal/compact TestSource_*",
	"compact-shortened-signals-tui-cell-style": "internal/compact TestPath_InitialsIntermediateSegmentsWhenTight (Shortened=true) + internal/tui TestProjectsModel_ViewFitsAt80Cols",
	"tui-projects-fits-at-80-cols":             "internal/tui TestProjectsModel_ViewFitsAt80Cols + TestSessionsModel_ViewFitsAt80Cols",
	"tui-shows-full-form-when-roomy":           "internal/tui TestProjectsModel_ViewShowsFullPathWhenRoomy",
}

func TestScenariosHaveCoverage(t *testing.T) {
	f, err := os.Open("../../scenarios.jsonl")
	if err != nil {
		t.Fatalf("open scenarios.jsonl: %v", err)
	}
	defer func() { _ = f.Close() }()

	fromFile := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var s struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(text), &s); err != nil {
			t.Fatalf("scenarios.jsonl line %d: %v", line, err)
		}
		if s.Name == "" {
			t.Fatalf("scenarios.jsonl line %d: missing name", line)
		}
		fromFile[s.Name] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read scenarios.jsonl: %v", err)
	}

	for name := range fromFile {
		if _, ok := scenarioCoverage[name]; !ok {
			t.Errorf("scenario %q has no coverage entry — add a test, then map it in scenarioCoverage", name)
		}
	}
	for name := range scenarioCoverage {
		if !fromFile[name] {
			t.Errorf("stale coverage entry %q — scenario no longer exists in scenarios.jsonl", name)
		}
	}
}
