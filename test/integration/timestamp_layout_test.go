// ABOUTME: Enforces that only internal/db/timestamps.go decides how the archive's DATETIME columns are read and written.
// ABOUTME: Bans time.Parse and storage-shaped layout literals everywhere else, so a second layout list cannot quietly appear.

package integration

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// timestampLayoutAllowlist lists the files allowed to spell a timestamp
// layout themselves, or to call time.Parse at all.
//
// Everything else must go through db.ParseTimestamp / db.FormatTimestamp.
// The rule exists because the archive now holds two eras of on-disk
// rendering — pre-migration-014 Go time.Time.String() and the post-014
// canonical RFC3339-UTC-with-padded-nanoseconds — and *every* reader has to
// accept both, forever. A second list is a second place to forget one, and
// the symptom is not an error: it is a search that silently misses rows.
//
// Adding a file here is a code-review decision. The question to ask is "is
// this parsing a value this archive wrote?" — if yes, it belongs in
// db.ParseTimestamp, not here.
var timestampLayoutAllowlist = []string{
	// The canonical definition: timestampLayout, FormatTimestamp and the
	// one accepted-layout list ParseTimestamp walks.
	"internal/db/timestamps.go",

	// Not an archive timestamp. MCP's protocol version is a date-shaped
	// identifier ("2026-06-18") that the handshake compares against the
	// versions this server speaks; it never touches a DATETIME column.
	"internal/mcp/server.go",

	// The *input* side. These read the timestamp field out of a foreign
	// transcript — Claude Code's, Codex's, jeff's JSONL — which is a format
	// those tools define and ccvault does not control. What comes back is
	// handed to the write path, which renders it with db.FormatTimestamp.
	// Parsing someone else's wire format is a different job from reading
	// back a value this archive stored.
	"pkg/parser/parser.go",
	"pkg/adapter/",

	// Human input, in the human's zone. parseDate turns what a person typed
	// into a search box — "2026-01-14", "Jan 2, 2026", "yesterday" — into the
	// local midnight they meant. Nothing it parses came out of a column, and
	// accepting the archive's storage renderings here would mean accepting a
	// nanosecond field from a user, which is not a kindness.
	"internal/search/query.go",

	// systemd's wire format, not the archive's. `systemctl show` renders
	// ExecMainExitTimestamp as "Wed 2026-10-28 09:15:00 UTC", in the
	// machine's own zone, and that is the value parseSystemdTimestamp reads.
	// Nothing it touches came out of a DATETIME column: the archive's own
	// staleness is read by db.LastSyncedAt, which goes through
	// db.ParseTimestamp like everything else.
	"internal/schedule/systemd.go",

	// Round-trips its own rendering. periodTerms re-reads the "2006-01-02"
	// that periodFilterDate just produced, to walk a calendar range a day at
	// a time. Both ends are this file's own constant; no stored value is
	// involved.
	"internal/search/period.go",
}

// storageShapedLayout is the fragment that distinguishes a layout for the
// archive's own storage from a layout for display.
//
// Display formats in this repo are space-separated and truncated —
// "2006-01-02 15:04", "2006-01-02 15:04:05 MST" — and belong to the UI that
// prints them. The T-separated ISO form is only ever the on-disk rendering,
// so a literal containing it outside the allowlist is someone writing a
// second copy of the storage layout.
const storageShapedLayout = "2006-01-02T15:04:05"

// TestTimestampLayoutEnforcement scans every tracked non-test .go file and
// fails if a file outside the allowlist calls time.Parse / time.ParseInLocation
// or spells a storage-shaped layout literal.
//
// Same shape as TestProjectRefEnforcement, and for the same reason: a "one
// way to do it" rule that nothing checks is a rule that holds until the next
// person who has not read this comment.
func TestTimestampLayoutEnforcement(t *testing.T) {
	repoRoot := findRepoRoot(t)

	var violations []string
	fset := token.NewFileSet()

	for _, rel := range trackedGoFiles(t, repoRoot) {
		// Tests construct layouts to build fixtures in the old renderings on
		// purpose — that is how migration 014's conversion is verified at
		// all. The discipline is about production code.
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		if hasPrefixIn(rel, timestampLayoutAllowlist) {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(repoRoot, rel), nil, parser.SkipObjectResolution)
		if err != nil {
			// Malformed Go isn't this test's job to flag.
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if name, ok := timePackageCall(node.Fun); ok &&
					(name == "Parse" || name == "ParseInLocation") {
					violations = append(violations,
						where(fset, node.Pos(), rel)+": time."+name+
							" — use db.ParseTimestamp")
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(node.Value)
				if err != nil {
					return true
				}
				if strings.Contains(text, storageShapedLayout) {
					violations = append(violations,
						where(fset, node.Pos(), rel)+": "+strconv.Quote(text)+
							" — use db.FormatTimestamp / db.ParseTimestamp")
				}
			}
			return true
		})
	}

	if len(violations) > 0 {
		t.Errorf(
			"timestamp layouts are decided outside internal/db/timestamps.go.\n\n"+
				"The archive holds two eras of on-disk rendering and every reader has to\n"+
				"accept both; a second layout list means a value parses in one package and\n"+
				"not in another, which surfaces as a search that silently misses rows.\n"+
				"Call db.ParseTimestamp / db.FormatTimestamp, or — if this genuinely is not\n"+
				"an archive timestamp — add the file to timestampLayoutAllowlist with a\n"+
				"justification.\n\nViolations:\n  %s",
			strings.Join(violations, "\n  "),
		)
	}
}

// timePackageCall reports the function name when expr is a call on an
// identifier named "time", e.g. time.Parse.
//
// Identifier-level, like TestProjectRefEnforcement's selector check: a local
// variable shadowing the package name would be a false positive, which is the
// right side to err on — a false positive costs an allowlist entry and a
// sentence in review, a false negative costs a silently unfindable row.
func timePackageCall(expr ast.Expr) (string, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "time" {
		return "", false
	}
	return sel.Sel.Name, true
}

func where(fset *token.FileSet, pos token.Pos, rel string) string {
	return rel + ":" + strconv.Itoa(fset.Position(pos).Line)
}

func hasPrefixIn(rel string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// TestCLI_StatsStrictReportsNonCanonicalTimestamps is #144 through the real
// binary.
//
// Migration 014 leaves a value it does not recognise exactly as it found it —
// the right call for a migration, and the reason a readable-but-non-canonical
// row can sit in the archive forever with nothing noticing. Everything built
// on the layout assumes it away: ORDER BY, MIN()/MAX() and the date predicate
// compare this text, and search_period slices the calendar date out of it by
// character position.
//
// So --strict reports it. It does not refuse it: the value is readable, an
// import could legitimately carry it, and the archive has repeatedly been
// better served by the user seeing a problem than by a guard that blocks a
// legitimate operation.
func TestCLI_StatsStrictReportsNonCanonicalTimestamps(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)
	f.Run(t, "sync")

	// A freshly synced archive is canonical, and the report says what it
	// covered rather than only that it found nothing.
	clean := f.Run(t, "stats", "--strict")
	for _, want := range []string{"Timestamp layout:", "turns.timestamp"} {
		if !strings.Contains(clean.Stdout, want) {
			t.Errorf("stats --strict does not mention %q on a clean archive:\n%s", want, clean.Stdout)
		}
	}

	const odd = "2026-01-01 00:00:01"
	setTurnTimestamp(t, f.DataDir, odd)

	// The existing checks have nothing to say about it: SQLite reads it, so
	// the #33 guard passes, and the index is sound, so --strict's FTS verdict
	// passes. That is what makes it worth reporting separately.
	bad := f.TryRun(t, "stats", "--strict")
	if bad.Err != nil {
		t.Fatalf("stats --strict failed over a readable non-canonical value; "+
			"#144 chose reporting over enforcement:\nstdout:\n%s\nstderr:\n%s", bad.Stdout, bad.Stderr)
	}
	for _, want := range []string{"turns.timestamp", odd} {
		if !strings.Contains(bad.Stdout, want) {
			t.Errorf("stats --strict does not surface %q:\n%s", want, bad.Stdout)
		}
	}

	// --json has to carry the same finding, or an agent reading the report is
	// told nothing.
	var report struct {
		TimestampLayout struct {
			OK        bool  `json:"ok"`
			OffLayout int64 `json:"off_layout"`
			Columns   []struct {
				Table     string `json:"table"`
				Column    string `json:"column"`
				Rows      int64  `json:"rows"`
				OffLayout int64  `json:"off_layout"`
				Sample    string `json:"sample,omitempty"`
			} `json:"columns"`
		} `json:"timestamp_layout"`
	}
	f.Run(t, "stats", "--strict", "--json").JSON(t, &report)
	if report.TimestampLayout.OK {
		t.Errorf("stats --strict --json reports ok over a non-canonical row: %+v", report.TimestampLayout)
	}
	if report.TimestampLayout.OffLayout != 1 {
		t.Errorf("off_layout = %d, want 1", report.TimestampLayout.OffLayout)
	}

	// Every DATETIME column, not just turns.timestamp.
	covered := map[string]bool{}
	var found bool
	for _, c := range report.TimestampLayout.Columns {
		covered[c.Table+"."+c.Column] = true
		if c.Table == "turns" && c.Column == "timestamp" {
			found = true
			if c.Sample != odd {
				t.Errorf("turns.timestamp sample = %q, want %q", c.Sample, odd)
			}
		}
	}
	if !found {
		t.Errorf("the JSON report does not name turns.timestamp: %+v", report.TimestampLayout.Columns)
	}
	for _, want := range []string{
		"sessions.started_at", "sessions.source_mtime", "tool_uses.timestamp",
		"source_files.mtime", "projects.first_seen_at",
	} {
		if !covered[want] {
			t.Errorf("the JSON report does not cover %s; #144 is about every DATETIME column", want)
		}
	}

	// And the default report stays cheap: this is a full scan of turns and
	// tool_uses, so it is opt-in like the strict FTS check it sits beside.
	if plain := f.Run(t, "stats"); strings.Contains(plain.Stdout, "Timestamp layout:") {
		t.Errorf("the default report runs the layout scan; it should be --strict only:\n%s", plain.Stdout)
	}
}

// setTurnTimestamp writes a raw value straight into turns.timestamp, the way
// an import or a hand repair would, bypassing db.FormatTimestamp. The update
// triggers still fire, so the FTS index stays consistent with the column —
// which is the point: nothing is broken, the value is just not canonical.
func setTurnTimestamp(t *testing.T, dataDir, value string) {
	t.Helper()

	pool, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ccvault.db"))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = pool.Close() }()

	res, err := pool.Exec(
		"UPDATE turns SET timestamp = ? WHERE rowid = (SELECT MIN(rowid) FROM turns)", value)
	if err != nil {
		t.Fatalf("seed a non-canonical turns.timestamp: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("seeded %d rows, want 1", n)
	}

	// It has to be readable, or it proves something weaker than #144 claims.
	var readable any
	if err := pool.QueryRow(
		"SELECT date(timestamp) FROM turns WHERE timestamp = ?", value).Scan(&readable); err != nil {
		t.Fatalf("date() on the seeded value: %v", err)
	}
	if readable == nil {
		t.Fatalf("%q is unreadable, so it is not a readable-but-non-canonical row", value)
	}
}
