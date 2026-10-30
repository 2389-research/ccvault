// ABOUTME: The package's public entry point — which mechanism this OS uses, and whether ccvault can drive it
// ABOUTME: Callers use these three rather than the platform_*.go internals

package schedule

// Platform names the scheduling mechanism on this OS — "launchd",
// "systemd", or the bare GOOS where there is none ccvault can drive.
func Platform() string { return platformName() }

// Supported reports whether ccvault can register a scheduled sync here.
//
// Asked separately from New so `status` can report an unsupported platform
// as a fact rather than as a failure to look.
func Supported() bool { return platformSupported() }

// New returns the Manager for this platform, or ErrUnsupportedPlatform.
func New() (Manager, error) { return newManager() }
