// ABOUTME: Platform selection on Linux — systemd user timers
// ABOUTME: The only linux-tagged file; all systemd logic lives in systemd.go so it compiles and tests everywhere

//go:build linux

package schedule

// platformName is the scheduling mechanism on this OS.
func platformName() string { return "systemd" }

// platformSupported reports whether ccvault can register a job here.
//
// True, with a caveat stated here because it is the thing a reader needs:
// `systemctl --user` needs a session bus, so a container or a headless CI
// runner with no logged-in session cannot register one. That shows up as an
// error from Install naming the missing bus, not as a silent no-op.
func platformSupported() bool { return true }

// newManager returns the systemd --user backend.
func newManager() (Manager, error) {
	return newSystemdManager()
}
