// ABOUTME: Merges another ccvault database into this one without data loss
// ABOUTME: Inserts rows absent by primary key and never overwrites a newer row

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MergeStats reports what a merge brought in.
type MergeStats struct {
	ProjectsInserted int64 `json:"projects_inserted"`
	SessionsInserted int64 `json:"sessions_inserted"`
	SessionsReplaced int64 `json:"sessions_replaced"`
	SessionsSkipped  int64 `json:"sessions_skipped"`
	TurnsInserted    int64 `json:"turns_inserted"`
	ToolUsesInserted int64 `json:"tool_uses_inserted"`
}

// mergeTables are the tables a merge reads from the incoming database. A file
// missing any of them isn't a ccvault archive.
var mergeTables = []string{"projects", "sessions", "turns", "tool_uses"}

// mergeRequiredColumns lists, per table, the columns that must exist in BOTH
// databases for the merge to produce complete rows. Everything else is merged
// on a best-effort intersection, so an archive written by an older or newer
// schema still imports — it just carries over the columns the two share.
var mergeRequiredColumns = map[string][]string{
	"projects":  {"path", "display_name"},
	"sessions":  {"id", "project_id", "started_at", "source_file"},
	"turns":     {"id", "session_id", "type", "timestamp"},
	"tool_uses": {"session_id", "tool_name", "timestamp"},
}

// MergeFrom merges the ccvault database at path into this one.
//
// Semantics, in one sentence: it only ever adds. A session absent here is
// inserted along with its turns and tool uses; a session present in both is
// left alone unless the incoming copy ended later, in which case the incoming
// copy replaces it wholesale. Nothing in the destination is deleted, so
// merging is the recovery path for an archive that a destructive rebuild
// discarded — and the way to consolidate archives from two machines.
//
// The incoming database is only ever read. The whole merge runs in one
// transaction, so a failure anywhere leaves the destination untouched.
//
// Sessions are re-pointed at the destination's own project rows by matching on
// project path, since projects.id is a local autoincrement key. An incoming
// session whose project row is missing from the incoming database (which would
// mean that archive is already inconsistent) imports with a NULL project_id
// rather than being dropped — the turns are the part worth keeping.
func (db *DB) MergeFrom(path string) (*MergeStats, error) {
	src, err := resolveMergeSource(path, db.path)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	// Pin one connection: ATTACH, the temp table, and the transaction all
	// live on a single connection, and the pool is capped at one anyway.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// ATTACH can't run inside a transaction, so it brackets the whole merge.
	// The path is caller-controlled and SQLite takes no bindings here, so
	// escape it the same way BackupTo escapes its VACUUM INTO target.
	escaped := strings.ReplaceAll(src, "'", "''")
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("ATTACH DATABASE '%s' AS incoming", escaped)); err != nil {
		return nil, fmt.Errorf("attach %s: %w", src, err)
	}
	defer func() {
		// Leaving the attachment behind would poison the pooled connection
		// for every later call, so detach even on the failure paths.
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE incoming")
	}()

	if err := verifyMergeSchema(ctx, conn); err != nil {
		return nil, err
	}

	columns := make(map[string][]string, len(mergeTables))
	for _, table := range mergeTables {
		cols, err := sharedColumns(ctx, conn, table)
		if err != nil {
			return nil, err
		}
		for _, required := range mergeRequiredColumns[table] {
			if !contains(cols, required) {
				return nil, fmt.Errorf("incoming database's %s table has no %q column; it is too different from this archive's schema to merge", table, required)
			}
		}
		columns[table] = cols
	}

	picks, skipped, err := pickSessions(ctx, conn)
	if err != nil {
		return nil, err
	}

	stats := &MergeStats{SessionsSkipped: skipped}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin merge transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
		// The temp tables outlive the transaction on a pooled connection,
		// so drop them explicitly rather than leaving them for the next caller.
		_, _ = conn.ExecContext(ctx, "DROP TABLE IF EXISTS temp.merge_pick")
		_, _ = conn.ExecContext(ctx, "DROP TABLE IF EXISTS temp.merge_tool_surplus")
	}()

	if err := mergeProjects(ctx, tx, columns["projects"], stats); err != nil {
		return nil, err
	}

	if len(picks) > 0 {
		if err := stagePicks(ctx, tx, picks); err != nil {
			return nil, err
		}
		if err := mergeSessions(ctx, tx, columns, picks, stats); err != nil {
			return nil, err
		}
	}

	// Aggregates are maintained additively elsewhere, so recompute them from
	// the sessions that now exist rather than trying to add the two archives'
	// counters together.
	if _, err := tx.ExecContext(ctx, reconcileAggregatesSQL); err != nil {
		return nil, fmt.Errorf("reconcile project aggregates: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit merge: %w", err)
	}
	committed = true

	return stats, nil
}

// Each token counter is COALESCEd before it is added for the same reason
// ReconcileProjectAggregates does it: `a + b + c + d` is NULL if any operand is
// NULL and SUM skips NULLs, so one session with one NULL counter would
// contribute zero instead of its three real ones. A merge is exactly where such
// a row arrives — the incoming archive need not have been written by this
// binary. See #110.
const reconcileAggregatesSQL = `
	UPDATE main.projects SET
		session_count = (
			SELECT COUNT(*) FROM main.sessions WHERE main.sessions.project_id = main.projects.id
		),
		total_tokens = (
			SELECT COALESCE(SUM(COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0)
				+ COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0)), 0)
			FROM main.sessions WHERE main.sessions.project_id = main.projects.id
		)`

// resolveMergeSource validates the path names a readable file that isn't the
// destination database itself, and returns it absolute.
func resolveMergeSource(path, destPath string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("read source database: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("source %s is a directory; pass the path to a ccvault.db file", abs)
	}

	// Merging a database into itself would duplicate every tool_use row (they
	// have no natural key) for no gain.
	destAbs, err := filepath.Abs(destPath)
	if err == nil && sameFile(abs, destAbs) {
		return "", fmt.Errorf("source and destination are the same database (%s)", abs)
	}

	return abs, nil
}

// sameFile compares two paths by identity where possible, falling back to a
// string compare when either can't be stat'd.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// verifyMergeSchema reports a usable error when the attached file isn't a
// ccvault archive, rather than letting a later INSERT fail cryptically.
func verifyMergeSchema(ctx context.Context, conn *sql.Conn) error {
	for _, table := range mergeTables {
		var n int
		err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM incoming.sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&n)
		if err != nil {
			return fmt.Errorf("inspect incoming schema: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("incoming database has no %q table; it does not look like a ccvault archive", table)
		}
	}
	return nil
}

// sharedColumns returns the columns the table has in both databases, in the
// destination's declaration order.
func sharedColumns(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	dest, err := tableColumns(ctx, conn, "main", table)
	if err != nil {
		return nil, err
	}
	incoming, err := tableColumns(ctx, conn, "incoming", table)
	if err != nil {
		return nil, err
	}

	present := make(map[string]bool, len(incoming))
	for _, c := range incoming {
		present[c] = true
	}

	var shared []string
	for _, c := range dest {
		if present[c] {
			shared = append(shared, c)
		}
	}
	return shared, nil
}

// tableColumns lists a table's column names. schema and table are package
// constants, never user input, so interpolating them into the PRAGMA is safe —
// PRAGMA takes no bindings.
func tableColumns(ctx context.Context, conn *sql.Conn, schema, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA %s.table_info(%s)", schema, table))
	if err != nil {
		return nil, fmt.Errorf("read %s.%s columns: %w", schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    sql.NullString
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &primaryKey); err != nil {
			return nil, fmt.Errorf("scan %s.%s columns: %w", schema, table, err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// sessionPick is one session the merge will take from the incoming database.
type sessionPick struct {
	id      string
	replace bool // true when it overwrites an older copy already here
}

// pickSessions decides which incoming sessions to take. A session absent from
// the destination is taken; one present in both is taken only when the
// incoming copy ended strictly later, which is what "never overwrite a newer
// row" reduces to. The comparison happens in Go because SQLite's stored
// datetime text carries a timezone offset — comparing it as a string would
// make an earlier moment look later across zones.
func pickSessions(ctx context.Context, conn *sql.Conn) ([]sessionPick, int64, error) {
	existing, err := sessionEndTimes(ctx, conn, "main")
	if err != nil {
		return nil, 0, err
	}
	incoming, err := sessionEndTimes(ctx, conn, "incoming")
	if err != nil {
		return nil, 0, err
	}

	var picks []sessionPick
	var skipped int64
	for id, incomingEnd := range incoming {
		currentEnd, present := existing[id]
		switch {
		case !present:
			picks = append(picks, sessionPick{id: id})
		case incomingEnd.After(currentEnd):
			picks = append(picks, sessionPick{id: id, replace: true})
		default:
			skipped++
		}
	}
	return picks, skipped, nil
}

// sessionEndTimes maps session id to ended_at for one attached schema. A NULL
// ended_at reads as the zero time, which sorts before every real timestamp —
// so a row with no end time never displaces one that has one.
func sessionEndTimes(ctx context.Context, conn *sql.Conn, schema string) (map[string]time.Time, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("SELECT id, ended_at FROM %s.sessions", schema))
	if err != nil {
		return nil, fmt.Errorf("read %s session timestamps: %w", schema, err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]time.Time)
	for rows.Next() {
		var id string
		var endedAt sql.NullTime
		if err := rows.Scan(&id, &endedAt); err != nil {
			return nil, fmt.Errorf("scan %s session timestamp: %w", schema, err)
		}
		if endedAt.Valid {
			result[id] = endedAt.Time
		} else {
			result[id] = time.Time{}
		}
	}
	return result, rows.Err()
}

// mergeProjects inserts projects whose path isn't here yet. Existing project
// rows are left exactly as they are; their counters get recomputed at the end.
func mergeProjects(ctx context.Context, tx *sql.Tx, cols []string, stats *MergeStats) error {
	// projects.id is a local autoincrement key, so it is never carried over —
	// sessions are re-pointed at the destination's own ids instead.
	cols = without(cols, "id")

	query := fmt.Sprintf(`
		INSERT INTO main.projects (%s)
		SELECT %s FROM incoming.projects i
		WHERE NOT EXISTS (SELECT 1 FROM main.projects m WHERE m.path = i.path)`,
		columnList(cols), qualify("i", cols))

	res, err := tx.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("merge projects: %w", err)
	}
	stats.ProjectsInserted, _ = res.RowsAffected()
	return nil
}

// stagePicks loads the chosen session ids into a temp table so the four bulk
// statements below can join against them instead of binding 17,000 parameters.
func stagePicks(ctx context.Context, tx *sql.Tx, picks []sessionPick) error {
	if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS temp.merge_pick"); err != nil {
		return fmt.Errorf("clear merge staging table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "CREATE TABLE temp.merge_pick (id TEXT PRIMARY KEY)"); err != nil {
		return fmt.Errorf("create merge staging table: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO temp.merge_pick (id) VALUES (?)")
	if err != nil {
		return fmt.Errorf("prepare merge staging insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, p := range picks {
		if _, err := stmt.ExecContext(ctx, p.id); err != nil {
			return fmt.Errorf("stage session %s: %w", p.id, err)
		}
	}
	return nil
}

// mergeSessions copies the staged sessions and their turns and tool uses,
// replacing any older copy's children first so nothing is duplicated.
func mergeSessions(ctx context.Context, tx *sql.Tx, columns map[string][]string, picks []sessionPick, stats *MergeStats) error {
	for _, p := range picks {
		if p.replace {
			stats.SessionsReplaced++
		} else {
			stats.SessionsInserted++
		}
	}

	// An older copy's rows must go before the incoming ones land or the
	// session ends up with both sets.
	for _, table := range []string{"tool_uses", "turns"} {
		query := fmt.Sprintf(
			"DELETE FROM main.%s WHERE session_id IN (SELECT id FROM temp.merge_pick)", table)
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("clear existing %s for merged sessions: %w", table, err)
		}
	}

	// Clearing tool_uses by session_id is not enough to leave no orphan: a row
	// can be filed under a session its turn does not belong to, and the DELETE
	// above cannot reach those. 1,281 rows on the author's archive were in
	// exactly that state before migration 011 repaired them, and that is how
	// #87's 6,378 orphans were made. The turns are gone as of the loop above,
	// so this reaches the rows by the turn they belong to instead.
	//
	// Matched on the uuid alone, deliberately, even though a turn's identity is
	// (session_id, id) since migration 012. Scoping this to the session would
	// also delete a row that a pre-011 archive filed under the wrong session —
	// the state migration 011 exists to *repair* — and an import is the one
	// path that still meets archives in that state. Only a row whose uuid no
	// turn in the destination holds at all is taken as an orphan here.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM main.tool_uses WHERE turn_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM main.turns t WHERE t.id = main.tool_uses.turn_id)`); err != nil {
		return fmt.Errorf("clear tool uses whose turns the merge removed: %w", err)
	}

	// projects.id differs between archives, so the incoming project_id is
	// translated through the project's path — the key that means the same
	// thing in both databases.
	sessionCols := without(columns["sessions"], "project_id")
	sessionQuery := fmt.Sprintf(`
		INSERT OR REPLACE INTO main.sessions (project_id, %s)
		SELECT (SELECT p.id FROM main.projects p WHERE p.path = ip.path), %s
		FROM incoming.sessions i
		JOIN temp.merge_pick k ON k.id = i.id
		LEFT JOIN incoming.projects ip ON ip.id = i.project_id`,
		columnList(sessionCols), qualify("i", sessionCols))
	if _, err := tx.ExecContext(ctx, sessionQuery); err != nil {
		return fmt.Errorf("merge sessions: %w", err)
	}

	// An archive written before migration 008 has no ordinal column, and
	// sharedColumns drops whatever the incoming database lacks — so without
	// this every imported turn would land on the column's DEFAULT 0 and the
	// second turn in a session would violate the unique index, turning a
	// recovery import into a hard failure. That archive is the main thing
	// `ccvault import` is pointed at: a copy taken off another machine, or a
	// backup predating this column.
	//
	// The position is computed the same way migration 008's backfill computes
	// it — rowid order within the session, which is the order that archive's
	// parser read the file in. The partition is complete because the join
	// takes a session's turns all or nothing.
	turnCols := columns["turns"]
	turnInsert, turnSelect := columnList(turnCols), qualify("i", turnCols)
	if !contains(turnCols, "ordinal") {
		turnInsert += ", " + quoteIdent("ordinal")
		turnSelect += ", ROW_NUMBER() OVER (PARTITION BY i.session_id ORDER BY i.rowid) - 1"
	}

	// A turn uuid is not unique to a session: a resumed transcript copies the
	// earlier session's lines verbatim, so the same uuid arrives in two files.
	// Since migration 012 that is what the schema says too — a turn is
	// identified by (session_id, id) — so the rows to clear are the picked
	// sessions' own copies, matched on both columns.
	//
	// It used to match on the uuid alone, on the reasoning that an incoming
	// turn "can collide with a row belonging to a session this merge never
	// picked". It can, and that row is that session's own turn: deleting it is
	// #92, the defect that cost the author's archive 31,864 turns. The DELETE
	// above has already cleared the picked sessions' turns, and after it there
	// is nothing left for this to reach — it stays because the INSERT that
	// follows must not be able to conflict, and because a session the merge
	// picked may hold turns under a source_file the loop above did not cover.
	//
	// Deleted here with a statement of our own rather than left to INSERT OR
	// REPLACE, for the reason internal/db/turns.go's deleteTurnConflictsSQL
	// sets out: a REPLACE's implicit delete reaches the turns_ad trigger only
	// when PRAGMA recursive_triggers is on, so an index that depends on it is
	// an index any writer without the pragma can shred. Position collisions
	// need no handling — the only rows that could hold a picked session's
	// position are that session's own, and they are already gone.
	//
	// Their tool uses go first, for the reason above: a turn that is deleted
	// rather than replaced must not leave rows pointing at it.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM main.tool_uses WHERE EXISTS (
			SELECT 1 FROM incoming.turns i
			JOIN temp.merge_pick k ON k.id = i.session_id
			WHERE i.id = main.tool_uses.turn_id
			  AND i.session_id = main.tool_uses.session_id
		)`); err != nil {
		return fmt.Errorf("clear tool uses of the turns the merge is about to replace: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM main.turns WHERE EXISTS (
			SELECT 1 FROM incoming.turns i
			JOIN temp.merge_pick k ON k.id = i.session_id
			WHERE i.id = main.turns.id
			  AND i.session_id = main.turns.session_id
		)`); err != nil {
		return fmt.Errorf("clear turns the merge is about to replace: %w", err)
	}

	turnQuery := fmt.Sprintf(`
		INSERT INTO main.turns (%s)
		SELECT %s FROM incoming.turns i
		JOIN temp.merge_pick k ON k.id = i.session_id`,
		turnInsert, turnSelect)
	res, err := tx.ExecContext(ctx, turnQuery)
	if err != nil {
		return fmt.Errorf("merge turns: %w", err)
	}
	stats.TurnsInserted, _ = res.RowsAffected()

	// Same gap on the sessions side, and the turns just landed, so the cursor
	// is derivable rather than lost. Only ever fills a blank: a session whose
	// incoming row carried the column keeps what it brought.
	if _, err := tx.ExecContext(ctx, `
		UPDATE main.sessions
		SET last_entry_uuid = (
			SELECT t.id FROM main.turns t
			WHERE t.session_id = main.sessions.id
			ORDER BY t.ordinal DESC
			LIMIT 1
		)
		WHERE id IN (SELECT id FROM temp.merge_pick)
		  AND last_entry_uuid IS NULL`); err != nil {
		return fmt.Errorf("fill last_entry_uuid for merged sessions: %w", err)
	}

	// tool_uses.id is a local autoincrement key; the destination assigns its own.
	toolCols := without(columns["tool_uses"], "id")
	toolInsert, toolSelect := columnList(toolCols), qualify("i", toolCols)

	// Same gap migration 008 left on turns.ordinal, for the same reason:
	// sharedColumns drops a column the incoming database lacks, so an archive
	// written before migration 011 would land every call of a turn on
	// turn_ordinal's DEFAULT 0 and the second one would violate the unique
	// index — turning a recovery import into a hard failure. The position is
	// computed the way migration 011's backfill computes it, rowid order within
	// the turn, which is the order that archive's parser read the message's
	// blocks in. Window functions are evaluated after the WHERE below, so the
	// numbering is gapless over the rows that actually land.
	if !contains(toolCols, "turn_ordinal") {
		toolInsert += ", " + quoteIdent("turn_ordinal")
		toolSelect += ", ROW_NUMBER() OVER (PARTITION BY i.turn_id ORDER BY i.rowid) - 1"
	}

	// Only calls whose turn is in this archive. main.turns holds everything the
	// merge brought in by now, so this is the join that stops an import from
	// adding to #87's orphan count: an incoming call can name a turn belonging
	// to a session the merge did not pick, and importing it would leave a row
	// nothing can reach.
	//
	// Joined on the uuid alone rather than on the turn's full identity, for the
	// reason the orphan pass above is: an incoming archive written before
	// migration 011 can file a call under a session its turn does not belong
	// to, and requiring the session to match would drop those rows instead of
	// importing them. The guard this join is here for — "is this call's turn in
	// the archive at all" — is answered by the uuid.
	toolWhere := `
		JOIN temp.merge_pick k ON k.id = i.session_id
		JOIN main.turns mt ON mt.id = i.turn_id`

	// And not the rows migration 011 would have deleted. A pre-011 archive
	// carries its duplicates, and #82's duplicates came from importing exactly
	// such an archive — so the rule that cleaned them up has to apply on the
	// way in too, or `ccvault import` re-creates them. Same rule, same shape,
	// staged into a temp table for the reason the migration stages it: written
	// as a correlated subquery it needs an index on tool_uses(turn_id) that an
	// archive predating migration 010 does not have, and without one SQLite
	// seeks on tool_name instead and the cost goes quadratic in the size of the
	// largest tool.
	//
	// Skipped when the incoming archive predates migration 009 and has none of
	// the three columns the rule reads. Such an archive cannot be deduplicated
	// at all — nothing in it says which of two rows is the same call — and the
	// ordinals still come out valid, so the import succeeds either way.
	if contains(toolCols, "tool_use_id") && contains(toolCols, "input_json") && contains(toolCols, "result_length") {
		if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS temp.merge_tool_surplus"); err != nil {
			return fmt.Errorf("drop merge tool surplus table: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE temp.merge_tool_surplus AS
			SELECT id FROM (
				SELECT id, tool_use_id, input_json, result_length,
					ROW_NUMBER() OVER (PARTITION BY turn_id, tool_name ORDER BY id) AS pos_in_group,
					ROW_NUMBER() OVER (PARTITION BY turn_id, tool_name, tool_use_id ORDER BY id) AS pos_in_id_group
				FROM incoming.tool_uses
			)
			WHERE (tool_use_id IS NOT NULL AND pos_in_id_group > 1)
			   OR (tool_use_id IS NULL
			       AND input_json IS NULL
			       AND result_length IS NULL
			       AND pos_in_group > 1)`); err != nil {
			return fmt.Errorf("stage the incoming archive's duplicate tool uses: %w", err)
		}
		toolWhere += `
		WHERE i.id NOT IN (SELECT id FROM temp.merge_tool_surplus)`
	}

	// Whatever the destination holds for the turns these calls belong to goes
	// first, so the INSERT cannot collide with the unique index — or, before
	// that index existed, quietly double the turn's calls.
	//
	// Matched on (session_id, turn_id), the turn's identity. It used to match
	// on turn_id alone, to reach "a destination row filed under a session this
	// merge never picked while naming a turn the incoming archive is supplying
	// calls for". Since migration 012 such a row is that session's own call on
	// its own copy of the turn, the unique index is scoped to the session too,
	// and so there is nothing to collide with — deleting it would be #92 one
	// table over. A destination row naming a turn nobody holds is an orphan,
	// and the pass near the top of this function is what removes those.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM main.tool_uses WHERE EXISTS (
			SELECT 1 FROM incoming.tool_uses i
			JOIN temp.merge_pick k ON k.id = i.session_id
			WHERE i.turn_id = main.tool_uses.turn_id
			  AND i.session_id = main.tool_uses.session_id
		)`); err != nil {
		return fmt.Errorf("clear tool uses the merge is about to replace: %w", err)
	}

	toolQuery := fmt.Sprintf(`
		INSERT INTO main.tool_uses (%s)
		SELECT %s FROM incoming.tool_uses i%s`,
		toolInsert, toolSelect, toolWhere)
	res, err = tx.ExecContext(ctx, toolQuery)
	if err != nil {
		return fmt.Errorf("merge tool uses: %w", err)
	}
	stats.ToolUsesInserted, _ = res.RowsAffected()

	return nil
}

// columnList renders columns for the INSERT target side.
func columnList(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quoteIdent(c)
	}
	return strings.Join(out, ", ")
}

// qualify prefixes each column with a table alias for the SELECT side.
func qualify(alias string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = alias + "." + quoteIdent(c)
	}
	return strings.Join(out, ", ")
}

// quoteIdent renders a SQLite identifier safely: double quotes, with internal
// quotes doubled. Identifiers can't be bound as parameters, so they have to be
// interpolated; quoting keeps that honest. The names reaching here are already
// narrowed to the destination's own schema by sharedColumns, so this is a
// second line of defence rather than the only one.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func without(cols []string, drop string) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if c != drop {
			out = append(out, c)
		}
	}
	return out
}

func contains(cols []string, want string) bool {
	for _, c := range cols {
		if c == want {
			return true
		}
	}
	return false
}
