#!/bin/sh
# ABOUTME: Fails if total statement coverage has dropped below the floor in .coverage-floor
# ABOUTME: Reads a profile written by `make test-coverage`; run via `make coverage-check`

# Fail closed. Every way this script can fail to *measure* something -- a
# missing profile, a missing floor file, output it cannot parse -- exits
# non-zero rather than passing. A coverage gate that goes green when it could
# not read the coverage is the failure mode this repo keeps hitting (#160,
# #151, #170): each reported success while delivering something else.
set -eu

profile="${1:-coverage.out}"
floor_file="${2:-.coverage-floor}"

# Suggest raising the floor once coverage sits this far above it. Advisory
# only -- it prints and never changes the exit status, because failing a build
# for being *too well tested* would teach people to delete tests.
suggest_margin=2.0

die() {
	echo "coverage-floor: $*" >&2
	exit 1
}

[ -f "$floor_file" ] || die "no floor file at $floor_file"
[ -f "$profile" ] || die "no coverage profile at $profile (run \`make test-coverage\` first)"

# Strip comments and blank lines; exactly one bare number must remain. Two
# numbers would mean guessing which one is the floor, and a guess here is how
# a gate ends up enforcing something other than what it says.
floor=$(sed 's/#.*//' "$floor_file" | tr -d '[:blank:]' | grep -v '^$' || true)
case $(printf '%s\n' "$floor" | grep -c .) in
0) die "$floor_file has no number in it" ;;
1) ;;
*) die "$floor_file has more than one number in it: $(printf '%s' "$floor" | tr '\n' ' ')" ;;
esac
# Validated with a case glob rather than a regex so this holds under any sh:
# digits, optionally one dot and more digits. Rejects "67.6%", "-1", "abc".
case "$floor" in
"" | *[!0-9.]* | *.*.*) die "floor $floor is not a bare decimal number" ;;
.* | *.) die "floor $floor is not a bare decimal number" ;;
esac

# The total is computed from the profile directly rather than read off
# `go tool cover -func`, and that is deliberate -- the two disagree here by
# six points.
#
# `go tool cover -func` attributes blocks to functions by parsing the source
# for `*ast.FuncDecl` extents, and a block belonging to no top-level function
# declaration is dropped from its total. cmd/ccvault/main.go declares sixteen
# cobra commands as package-level `var x = &cobra.Command{RunE: func(...)}`,
# so every one of those command bodies lives in a *var initializer*, not in a
# FuncDecl. Measured: `-func` sees ~465 of cmd/ccvault's 1066 statements and
# reports the package at 64.9%, while `go test` reports 28.7% over all of
# them. The ~600 statements it silently drops are the least-tested code in
# the repo, so gating on `-func` would mean a new untested cobra command
# barely moves the number -- #151's mistake with a different mechanism.
#
# A profile block is `file:startLine.col,endLine.col numStmt count`. Summing
# numStmt over all blocks, and over those with a non-zero count, is the whole
# computation. Cross-checked against `go test`'s own per-package percentages:
# all 21 packages agree exactly, which is the property `-func` lacks.
#
# Every field is shape-checked before use, rather than only counted. Checking
# the count alone is not enough: `garbage line here` has three fields and
# would be read as a block of zero statements, which sums to a real-looking
# number instead of an error. Each line must be a position followed by two
# integers or the parse fails.
read_total=$(awk '
	/^mode:/ { next }
	NF == 0  { next }
	NF != 3 ||
	$1 !~ /^.+:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/ ||
	$2 !~ /^[0-9]+$/ ||
	$3 !~ /^[0-9]+$/ { bad++; next }
	{ total += $2; if ($3 > 0) covered += $2 }
	END {
		if (bad > 0)     { print "BAD " bad; exit }
		if (total == 0)  { print "EMPTY"; exit }
		printf "%.1f %d %d\n", 100 * covered / total, covered, total
	}
' "$profile")

case "$read_total" in
BAD\ *) die "$profile has $(printf '%s' "$read_total" | cut -d' ' -f2) line(s) that are not coverage blocks" ;;
EMPTY) die "$profile contains no coverage blocks" ;;
esac

total=$(printf '%s\n' "$read_total" | cut -d' ' -f1)
covered=$(printf '%s\n' "$read_total" | cut -d' ' -f2)
statements=$(printf '%s\n' "$read_total" | cut -d' ' -f3)
case "$total" in
"" | *[!0-9.]* | *.*.*) die "could not compute a percentage from $profile" ;;
esac

# Float comparison in awk, because sh does integer arithmetic only. Exit codes
# rather than stdout so a mangled awk invocation cannot read as a pass.
if awk -v t="$total" -v f="$floor" 'BEGIN { exit !(t < f) }'; then
	deficit=$(awk -v t="$total" -v f="$floor" 'BEGIN { printf "%.1f", f - t }')
	echo "coverage-floor: FAIL -- total coverage $total% ($covered/$statements statements) is below the floor of $floor% (down $deficit points)" >&2
	echo "" >&2
	echo "Coverage was lost. Find what stopped being exercised:" >&2
	echo "  go tool cover -func=$profile | awk '\$3 == \"0.0%\"'" >&2
	echo "" >&2
	echo "Add tests for it, or -- if tests were removed on purpose -- lower the" >&2
	echo "number in $floor_file in its own commit and say which ones and why." >&2
	exit 1
fi

echo "coverage-floor: OK -- total coverage $total% ($covered/$statements statements) meets the floor of $floor%"

if awk -v t="$total" -v f="$floor" -v m="$suggest_margin" 'BEGIN { exit !(t - f >= m) }'; then
	echo "coverage-floor: coverage is $(awk -v t="$total" -v f="$floor" 'BEGIN { printf "%.1f", t - f }') points above the floor; consider raising $floor_file to $total"
fi
