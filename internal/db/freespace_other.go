// ABOUTME: Free disk space probe fallback for platforms without statfs
// ABOUTME: Reports that the space preflight cannot run, so callers skip it

//go:build !unix

package db

// availableBytes is a variable so the compaction tests can simulate a nearly
// full filesystem, which a temp dir cannot produce on demand.
var availableBytes = freeBytes

func freeBytes(string) (int64, error) {
	return 0, errFreeSpaceUnsupported
}
