// ABOUTME: Unit tests for the compact package.
// ABOUTME: Verifies the compaction ladder + Shortened signaling for Path/Source/Model/Date.

package compact

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// This test file lives in the same package as compact.go, so it uses
// the package's own visLen for width assertions rather than redeclaring.

// --- Tilde ---------------------------------------------------------------

func TestTilde_AbbreviatesHomePrefix(t *testing.T) {
	t.Setenv("HOME", "/Users/testuser")
	got := Tilde("/Users/testuser/work/ccvault")
	if got != "~/work/ccvault" {
		t.Errorf("Tilde = %q, want %q", got, "~/work/ccvault")
	}
}

func TestTilde_GuardsAgainstPrefixCollision(t *testing.T) {
	// HOME is a string prefix but not a parent directory — must not
	// abbreviate. This is the exact bug fresh-eyes review caught in PR #22.
	t.Setenv("HOME", "/Users/dyl")
	got := Tilde("/Users/dylan/repo")
	if got != "/Users/dylan/repo" {
		t.Errorf("Tilde = %q, want %q (no abbreviation)", got, "/Users/dylan/repo")
	}
}

func TestTilde_ExactHomeBecomesTildeOnly(t *testing.T) {
	t.Setenv("HOME", "/Users/testuser")
	if got := Tilde("/Users/testuser"); got != "~" {
		t.Errorf("Tilde = %q, want %q", got, "~")
	}
}

// --- Path ----------------------------------------------------------------

func TestPath_UntouchedWhenItFits(t *testing.T) {
	t.Setenv("HOME", "/Users/testuser")
	// Full form is 30 chars; maxWidth 40 gives room.
	r := Path("/Users/testuser/work/2389/ccvault", 40)
	if r.Text != "~/work/2389/ccvault" {
		t.Errorf("Path.Text = %q", r.Text)
	}
	// Tilde substitution alone doesn't count as compaction that costs
	// information — it's just a well-known abbreviation.
	if r.Shortened {
		t.Errorf("Path.Shortened = true, want false (tilde-only counts as full)")
	}
}

func TestPath_InitialsIntermediateSegmentsWhenTight(t *testing.T) {
	t.Setenv("HOME", "/Users/testuser")
	// ~/work/2389/ccvault (19 chars) at maxWidth 16.
	// Expected ladder: initial 1 segment → ~/w/2389/ccvault (16). Fits.
	r := Path("/Users/testuser/work/2389/ccvault", 16)
	if r.Text != "~/w/2389/ccvault" {
		t.Errorf("Path.Text = %q, want %q", r.Text, "~/w/2389/ccvault")
	}
	if !r.Shortened {
		t.Errorf("Path.Shortened = false, want true (segment was initialed)")
	}
}

func TestPath_KeepsLastSegmentAsLongAsPossible(t *testing.T) {
	t.Setenv("HOME", "/Users/testuser")
	// At maxWidth 13, ~/w/2/ccvault (13 chars) should be the choice —
	// last segment preserved.
	r := Path("/Users/testuser/work/2389/ccvault", 13)
	if r.Text != "~/w/2/ccvault" {
		t.Errorf("Path.Text = %q, want %q", r.Text, "~/w/2/ccvault")
	}
	if !r.Shortened {
		t.Errorf("Path.Shortened = false, want true")
	}
}

func TestPath_FallsBackToTildeTruncationWhenTiny(t *testing.T) {
	t.Setenv("HOME", "/Users/testuser")
	// Really narrow — even all-initials doesn't fit.
	r := Path("/Users/testuser/work/2389/ccvault", 4)
	// Just verify it fits and is shortened; exact form isn't a stable
	// contract at this extremity.
	if visLen(r.Text) > 4 {
		t.Errorf("Path.Text = %q (visLen %d), exceeds maxWidth 4", r.Text, visLen(r.Text))
	}
	if !r.Shortened {
		t.Errorf("Path.Shortened = false, want true")
	}
}

func TestPath_EmptyReturnsEmpty(t *testing.T) {
	if r := Path("", 20); r.Text != "" || r.Shortened {
		t.Errorf("Path('') = %v", r)
	}
}

func TestPath_ZeroWidthReturnsFullInput(t *testing.T) {
	// maxWidth of 0 means "no constraint" — return the natural form.
	// This lets callers pass through when they don't have a width yet.
	r := Path("/a/b/c", 0)
	if r.Text != "/a/b/c" || r.Shortened {
		t.Errorf("Path zero-width = %v, want unchanged", r)
	}
}

// --- Source --------------------------------------------------------------

func TestSource_ReturnsFullWhenItFits(t *testing.T) {
	r := Source("claude-code", 20)
	if r.Text != "claude-code" || r.Shortened {
		t.Errorf("Source at 20 = %v", r)
	}
}

func TestSource_UsesMediumShorthandUnderPressure(t *testing.T) {
	r := Source("claude-code", 8)
	if r.Text != "claude" {
		t.Errorf("Source at 8 = %q, want %q", r.Text, "claude")
	}
	if !r.Shortened {
		t.Errorf("Shortened = false, want true")
	}
}

func TestSource_UsesShortShorthandWhenTight(t *testing.T) {
	r := Source("claude-code", 4)
	if r.Text != "cc" {
		t.Errorf("Source at 4 = %q, want %q", r.Text, "cc")
	}
	if !r.Shortened {
		t.Errorf("Shortened = false, want true")
	}
}

func TestSource_UnknownAdapterEndTruncates(t *testing.T) {
	r := Source("some-unknown-adapter", 8)
	if !strings.HasSuffix(r.Text, "…") {
		t.Errorf("Source unknown at 8 = %q, want a …-suffixed truncation", r.Text)
	}
	if !r.Shortened {
		t.Errorf("Shortened = false, want true")
	}
	// Strengthened assertion (from adversarial review): the prefix must
	// match the input's actual prefix — not "cc" or some hardcoded shorthand
	// that would silently conflate an unknown adapter with claude-code.
	// Without this check, a bug like `return shortened("cc…")` for the
	// default branch passes.
	wantPrefix := "some-un" // first (maxWidth-1)=7 runes of "some-unknown-adapter"
	if !strings.HasPrefix(r.Text, wantPrefix) {
		t.Errorf("Source unknown truncation prefix = %q, want prefix %q (adapter name preserved)", r.Text, wantPrefix)
	}
}

func TestSource_UnknownAdapterMultiByte(t *testing.T) {
	// A source name with multibyte runes must NOT be byte-sliced —
	// byte-slice would produce an invalid UTF-8 sequence and mojibake.
	// Regression guard: bug found by adversarial reviewer.
	src := "αβγδε" // 10 bytes, 5 runes
	r := Source(src, 4)
	if !utf8.ValidString(r.Text) {
		t.Errorf("Source(%q, 4).Text = %q — not valid UTF-8 (byte-slice regression)", src, r.Text)
	}
	// At maxWidth 4, we expect 3 runes + "…" = "αβγ…".
	if r.Text != "αβγ…" {
		t.Errorf("Source(%q, 4).Text = %q, want %q", src, r.Text, "αβγ…")
	}
}

func TestPath_MultiByteSegments(t *testing.T) {
	// Segment initialing must use the first RUNE, not the first byte.
	// A path with Cyrillic characters was producing "Ð" (invalid UTF-8)
	// as the initial for "Ярослав" instead of "Я".
	// Regression guard: bug found by adversarial reviewer.
	path := "/Users/user/Ярослав/repo"
	r := Path(path, 15) // forces some initialing
	if !utf8.ValidString(r.Text) {
		t.Errorf("Path(%q, 15).Text = %q — invalid UTF-8 (byte-slice regression)", path, r.Text)
	}
	// Whatever the exact form, "Я" must appear if that segment got initialed
	// (or the segment must appear in full). It must NOT appear as "Ð".
	if strings.Contains(r.Text, "Ð") {
		t.Errorf("Path Cyrillic segment produced 'Ð' (byte-slice mojibake): %q", r.Text)
	}
}

// --- Model ---------------------------------------------------------------

func TestModel_ReturnsFullWhenItFits(t *testing.T) {
	r := Model("claude-opus-4-5-20251101", 30)
	if r.Text != "claude-opus-4-5-20251101" || r.Shortened {
		t.Errorf("Model at 30 = %v", r)
	}
}

func TestModel_StripsClaudePrefixWhenTight(t *testing.T) {
	// Full 24 chars; at maxWidth 20, stripping "claude-" gives 17 — fits.
	r := Model("claude-opus-4-5-20251101", 20)
	if r.Text != "opus-4-5-20251101" {
		t.Errorf("Model at 20 = %q, want %q", r.Text, "opus-4-5-20251101")
	}
	if !r.Shortened {
		t.Errorf("Shortened = false, want true")
	}
}

func TestModel_NeverDropsTrailingDatestamp(t *testing.T) {
	// opus-4-5 is a semantically different model from opus-4-5-20251101.
	// A "smart" compactor that dropped the datestamp would silently
	// conflate them. At maxWidth 8 we CAN'T fit the full stripped form,
	// so we must end-truncate visibly — never strip the tail.
	r := Model("claude-opus-4-5-20251101", 8)
	if r.Text == "opus-4-5" {
		t.Errorf("Model at 8 dropped datestamp: %q — this would conflate distinct models", r.Text)
	}
	if !strings.HasSuffix(r.Text, "…") {
		t.Errorf("Model at 8 = %q, want visible …-truncation", r.Text)
	}
	if visLen(r.Text) > 8 {
		t.Errorf("Model at 8 = %q (visLen %d), exceeds maxWidth", r.Text, visLen(r.Text))
	}
	if !r.Shortened {
		t.Errorf("Shortened = false, want true")
	}
}

// --- Date ----------------------------------------------------------------

func TestDate_FullISOFormWhenItFits(t *testing.T) {
	when := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
	r := Date(when, 15)
	if r.Text != "2026-02-14" || r.Shortened {
		t.Errorf("Date at 15 = %v", r)
	}
}

func TestDate_UsesTwoDigitYearWhenPressed(t *testing.T) {
	when := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
	r := Date(when, 8)
	if r.Text != "26-02-14" {
		t.Errorf("Date at 8 = %q, want %q", r.Text, "26-02-14")
	}
	if !r.Shortened {
		t.Errorf("Shortened = false, want true")
	}
}

func TestDate_NeverDropsMonthOrDay(t *testing.T) {
	// Under pressure below YY-MM-DD (8 chars), the doctrine is to return
	// EMPTY rather than a partial date that looks precise but silently
	// loses the day — "26-08…" reads as a real date and hides the fact
	// that 26-08-05 vs 26-08-15 are now indistinguishable. Callers pad
	// whitespace and the row visibly shows something's missing.
	when := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
	r := Date(when, 4)
	if r.Text != "" {
		t.Errorf("Date at 4 = %q, want empty (never emit a partial date)", r.Text)
	}
	if !r.Shortened {
		t.Errorf("Date at 4 must still mark Shortened so callers can style the empty cell")
	}
	// The intermediate boundary: exactly 7 (YY-MM-DD is 8) — must also return empty.
	r7 := Date(when, 7)
	if r7.Text != "" {
		t.Errorf("Date at 7 = %q, want empty (YY-MM-DD is 8 chars, no partial forms allowed)", r7.Text)
	}
	// Sanity: at exactly 8, we do get the YY-MM-DD form.
	r8 := Date(when, 8)
	if r8.Text != "26-02-14" {
		t.Errorf("Date at 8 = %q, want %q", r8.Text, "26-02-14")
	}
}

func TestDate_ZeroTimeReturnsEmpty(t *testing.T) {
	r := Date(time.Time{}, 20)
	if r.Text != "" || r.Shortened {
		t.Errorf("Date zero = %v, want empty non-shortened", r)
	}
}
