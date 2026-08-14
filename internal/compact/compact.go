// ABOUTME: Width-aware compaction helpers for paths, sources, models, dates.
// ABOUTME: Every helper returns Result{Text, Shortened} so callers can style compacted cells.

// Package compact holds the ladder of "make this thing fit in N chars, and
// tell me if it got shortened" helpers used across CLI and TUI. Each helper
// prefers the natural form when it fits; compaction happens only under
// pressure. The Shortened flag lets TUI callers apply a dim style so users
// know a value has been abbreviated and can go read the fuller form
// elsewhere.
//
// Discipline per field:
//
//	Path   — tilde substitution first, then progressively initial segments
//	         left-to-right (last segment preserved as long as possible).
//	         Final fallback is "…"+tail truncation. Always visually cued
//	         because the initialed form has a different shape.
//
//	Source — well-known adapter shorthands (claude-code → claude → cc,
//	         nanoclaw → nano → nc, etc.). Unknown sources end-truncate
//	         with "…" so nothing gets silently mapped to a wrong shorthand.
//
//	Model  — always strips the "claude-" prefix (universal, semantically
//	         safe). NEVER strips a trailing datestamp — opus-4-5-20251101
//	         is a genuinely different model from opus-4-5, and silent
//	         conflation is a correctness bug, not a display trade-off.
//	         Under further pressure, end-truncate with "…" so the user
//	         sees identity has been lost.
//
//	Date   — full ISO 8601 (2026-02-14) when it fits; compact YY-MM-DD
//	         (26-02-14) when squeezed. Never drops day or month.
package compact

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// visLen returns the visible column width of s (rune count). Compaction
// helpers work in visible columns because they render into fixed-width
// terminal cells, not byte buffers. Notably, "…" is 3 bytes but 1
// visible column — using len() would over-count and produce cells that
// still overflow maxWidth after "compaction".
func visLen(s string) int { return utf8.RuneCountInString(s) }

// runeSlice returns the first n runes of s as a string. Guards against
// the "byte-slice a UTF-8 string" bug that produces mojibake and
// invalid codepoints — a source name like "αβγδε" byte-sliced to [:3]
// yields "\xce\xb1\xce" (invalid UTF-8), whereas rune-slice to 3
// correctly yields "αβγ". Returns s unchanged when n is >= its rune
// count so callers can chain unconditionally.
func runeSlice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if n >= len(runes) {
		return s
	}
	return string(runes[:n])
}

// Result carries the compacted text plus a flag saying whether any
// compaction happened. TUI callers style the cell (dim) when Shortened
// is true so users know to look for the fuller form elsewhere.
type Result struct {
	Text      string
	Shortened bool
}

func full(text string) Result      { return Result{Text: text, Shortened: false} }
func shortened(text string) Result { return Result{Text: text, Shortened: true} }

// Tilde returns path with $HOME (or its equivalent for the caller's shell)
// abbreviated to "~". The separator boundary is honored — a HOME of
// "/Users/dyl" will NOT rewrite "/Users/dylan/x" to "~an/x". Extracted
// here so callers other than projectref.Inline can share the same
// boundary-safe implementation.
func Tilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// Path returns a form of path that fits within maxWidth characters.
// Never returns a longer string than the input. When maxWidth is zero
// or negative, returns the input untouched (Shortened=false).
func Path(path string, maxWidth int) Result {
	if maxWidth <= 0 || path == "" {
		return full(path)
	}

	tilded := Tilde(path)
	if visLen(tilded) <= maxWidth {
		// Tilde substitution changed the string but only in a "well-known"
		// way — that's not really a "shortened" compaction that costs
		// information, so we don't set Shortened just for tildes. Callers
		// that want to detect ANY difference from the original path can
		// compare directly.
		return full(tilded)
	}

	parts := strings.Split(tilded, "/")
	if len(parts) < 2 {
		return shortened(truncateFromLeft(tilded, maxWidth))
	}

	// Progressively initial segments from left, preserving as many
	// trailing segments in full as possible. Iterate from LEAST compact
	// to MOST compact — return the first form that fits so the user
	// sees the fullest possible path their width allows.
	for initialCount := 1; initialCount <= len(parts); initialCount++ {
		candidate := initialParts(parts, initialCount)
		if visLen(candidate) <= maxWidth {
			if candidate != tilded {
				return shortened(candidate)
			}
			return full(candidate)
		}
	}

	// Even all-initials was still too long. Fall back to "…" + tail.
	allInit := initialParts(parts, 0)
	if visLen(allInit) <= maxWidth {
		if allInit != tilded {
			return shortened(allInit)
		}
		return full(allInit)
	}
	return shortened(truncateFromLeft(allInit, maxWidth))
}

// initialParts joins parts with "/", keeping only the first RUNE of
// each part BEFORE preservePastIdx (in [0, len(parts)]). Empty parts
// and "~" are preserved as-is regardless of position — they carry
// structural meaning that a one-letter form can't.
//
// Rune-based first-character extraction so multi-byte segments (e.g.
// paths with Cyrillic, CJK, or emoji) produce a valid single-glyph
// initial rather than a mojibake byte prefix — /Users/user/Ярослав/repo
// initialed to ~/U/u/Я/repo, not ~/U/u/Ð/repo.
func initialParts(parts []string, preservePastIdx int) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		if i < preservePastIdx && visLen(p) > 1 && p != "~" && p != "" {
			first, _ := utf8.DecodeRuneInString(p)
			out[i] = string(first)
		} else {
			out[i] = p
		}
	}
	return strings.Join(out, "/")
}

// truncateFromLeft keeps the tail so the identifying end of the string
// remains readable. "…" is a single Unicode character; caller should be
// prepared for it to occupy 1 column in rendering.
func truncateFromLeft(s string, maxWidth int) string {
	if visLen(s) <= maxWidth {
		return s
	}
	if maxWidth <= 1 {
		return "…"
	}
	// Take the last (maxWidth-1) runes so we leave room for the "…".
	// Rune-based slicing so we don't split a UTF-8 codepoint mid-way.
	runes := []rune(s)
	return "…" + string(runes[len(runes)-(maxWidth-1):])
}

// Source returns a compact form of a source adapter name.
func Source(source string, maxWidth int) Result {
	if maxWidth <= 0 || visLen(source) <= maxWidth {
		return full(source)
	}

	// Well-known adapters get ladder-defined abbreviations. Only fire the
	// short form when the width bracket demands it, so a user with a
	// mid-width terminal sees "claude" rather than jumping to "cc".
	switch source {
	case "claude-code":
		if maxWidth >= 6 {
			return shortened("claude")
		}
		return shortened("cc")
	case "nanoclaw":
		if maxWidth >= 4 {
			return shortened("nano")
		}
		return shortened("nc")
	case "codex":
		if maxWidth >= 2 {
			return shortened("cx")
		}
	case "hex", "jeff":
		// Match the unknown-adapter branch — end-truncate with "…" so a
		// silent conflation ("jef" as truncated "jeff") isn't possible.
		if maxWidth <= 1 {
			return shortened("…")
		}
		return shortened(runeSlice(source, maxWidth-1) + "…")
	}

	// Unknown adapter: end-truncate with "…" so nothing gets mapped to a
	// wrong shorthand. Rune-based slice — a source name like "αβγδε"
	// (10 bytes / 5 runes) at maxWidth 4 must NOT byte-slice; that would
	// produce an invalid UTF-8 sequence.
	if maxWidth <= 1 {
		return shortened("…")
	}
	return shortened(runeSlice(source, maxWidth-1) + "…")
}

// Model shortens a model identifier. Only performs semantically SAFE
// operations — the "claude-" prefix strip is universal; a trailing
// datestamp is NEVER stripped because opus-4-5-20251101 is a genuinely
// different model from opus-4-5. Under further pressure the string is
// end-truncated with "…" so the user sees identity has been partially
// hidden rather than silently conflated.
func Model(model string, maxWidth int) Result {
	if maxWidth <= 0 || visLen(model) <= maxWidth {
		return full(model)
	}

	stripped := strings.TrimPrefix(model, "claude-")
	if stripped != model && visLen(stripped) <= maxWidth {
		return shortened(stripped)
	}

	// End-truncate the stripped-or-original form. Never strip trailing
	// datestamp / version fragment — silently losing that would conflate
	// distinct model releases.
	target := stripped
	if target == "" {
		target = model
	}
	if maxWidth <= 1 {
		return shortened("…")
	}
	// maxWidth-1 leaves room for the ellipsis; rune-based slice.
	runes := []rune(target)
	if len(runes) > maxWidth-1 {
		runes = runes[:maxWidth-1]
	}
	return shortened(string(runes) + "…")
}

// Date returns t formatted to fit within maxWidth. Full form is ISO 8601
// "2026-02-14" (10 chars); compact form is "YY-MM-DD" (8 chars). Never
// drops day or month — a date visible without a day would be a worse
// display than a fully-truncated one.
func Date(t time.Time, maxWidth int) Result {
	if t.IsZero() {
		return full("")
	}
	iso := t.Format("2006-01-02")
	if maxWidth <= 0 || visLen(iso) <= maxWidth {
		return full(iso)
	}
	// Compact YY-MM-DD form. Century is unambiguous in the ccvault context
	// (dev tool, no ancient sessions).
	yy := t.Format("06-01-02")
	if visLen(yy) <= maxWidth {
		return shortened(yy)
	}
	// The compact form doesn't fit either. Doctrine: never drop day or
	// month — a partial date like "26-08…" is worse than nothing because
	// it looks precise but silently loses the day (26-08-05 vs 26-08-15
	// are visually identical). Return an empty visibly-shortened cell so
	// callers pad whitespace instead of showing a misleading value.
	return shortened("")
}

// Truncate is a small helper for cases where a caller just wants an
// end-truncated string with an ellipsis. Not width-aware in the ladder
// sense; use the field-specific helpers when you can.
func Truncate(s string, maxWidth int) Result {
	if maxWidth <= 0 || visLen(s) <= maxWidth {
		return full(s)
	}
	if maxWidth == 1 {
		return shortened("…")
	}
	runes := []rune(s)
	if len(runes) > maxWidth-1 {
		runes = runes[:maxWidth-1]
	}
	return shortened(fmt.Sprintf("%s…", string(runes)))
}
