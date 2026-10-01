// ABOUTME: Free disk space probe for unix platforms, used by compaction preflight
// ABOUTME: Reports bytes available to an unprivileged process on a path's filesystem

//go:build unix

package db

import (
	"fmt"
	"math"
	"syscall"
)

// availableBytes is a variable so the compaction tests can simulate a nearly
// full filesystem, which a temp dir cannot produce on demand.
var availableBytes = freeBytes

// freeBytes reports the space available to this process on the filesystem
// holding path. Blocks reserved for root are excluded, so this is what a
// write can actually use.
func freeBytes(path string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}

	blockSize := int64(fs.Bsize)
	if blockSize <= 0 {
		return 0, fmt.Errorf("statfs %s: implausible block size %d", path, blockSize)
	}
	// Available blocks are unsigned and wider than the result. Nothing real
	// reports an exabyte of free space, but a bogus value must saturate
	// rather than wrap negative and sabotage the space preflight.
	if fs.Bavail > uint64(math.MaxInt64)/uint64(blockSize) {
		return math.MaxInt64, nil
	}
	return int64(fs.Bavail) * blockSize, nil //nolint:gosec // G115: bounded by the check above
}
