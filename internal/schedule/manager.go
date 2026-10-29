// ABOUTME: The Manager interface every platform backend implements, and the errors a caller must handle
// ABOUTME: Platform selection lives in platform_*.go; the backends themselves in launchd.go and systemd.go

package schedule

// Manager registers, removes and inspects the scheduled job on one platform.
//
// Install and Uninstall both re-inspect before returning, so a registration
// that the OS accepted and then dropped is an error rather than a success
// message. Inspect is the only source of truth `schedule status` consults.
type Manager interface {
	// Platform names the mechanism, for display: "launchd", "systemd".
	Platform() string

	// Install registers the job, replacing any existing registration under
	// the same label. Idempotent.
	Install(j Job) error

	// Uninstall removes the registration. Returns ErrNotRegistered when
	// there was nothing to remove.
	Uninstall(label string) error

	// Inspect reports what the OS currently holds for this label. A label
	// with no registration is a zero Registration and a nil error.
	Inspect(label string) (Registration, error)
}
