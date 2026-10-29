// ABOUTME: Platform selection everywhere else — a refusal, not a silent no-op
// ABOUTME: Windows Task Scheduler is deliberately NOT implemented; see the comment on platformSupported

//go:build !darwin && !linux

package schedule

import "runtime"

// platformName is the scheduling mechanism on this OS: there isn't one.
func platformName() string { return runtime.GOOS }

// platformSupported reports whether ccvault can register a job here.
//
// False, and Windows is the case that matters. Task Scheduler could be driven
// with schtasks.exe and the XML would be about as hard to generate as the
// plist — but nobody working on this can run it, and registration is the half
// that unit tests cannot stand in for. Shipping it would mean shipping a
// command whose success message was never once observed to be true.
//
// This repo has shipped three things that reported success while doing
// nothing (#160, #151, #170). A scheduler is the worst possible candidate for
// a fourth, because the whole premise is that nobody is watching it. So
// `ccvault schedule install` says plainly that this platform is not supported
// yet, and `status` reports the same rather than guessing at four facts it
// cannot establish.
func platformSupported() bool { return false }

// newManager has nothing to return here.
func newManager() (Manager, error) {
	return nil, ErrUnsupportedPlatform
}
