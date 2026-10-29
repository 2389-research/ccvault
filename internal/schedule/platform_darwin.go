// ABOUTME: Platform selection on macOS — launchd, verified on a real Mac
// ABOUTME: The only darwin-tagged file; all launchd logic lives in launchd.go so it compiles and tests everywhere

//go:build darwin

package schedule

import "os"

// platformName is the scheduling mechanism on this OS.
func platformName() string { return "launchd" }

// platformSupported reports whether ccvault can register a job here.
func platformSupported() bool { return true }

// newManager returns the launchd backend for the calling user.
//
// os.Getuid rather than a parsed `id -u`: the GUI domain is keyed on the real
// uid, and asking the process for its own uid cannot be wrong.
func newManager() (Manager, error) {
	return newLaunchdManager(os.Getuid())
}
