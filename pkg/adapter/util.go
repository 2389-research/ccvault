// ABOUTME: Shared utility functions for source adapters.
// ABOUTME: Provides common helpers like byte slice copying to avoid scanner buffer aliasing.

package adapter

// MakeCopy returns a copy of the byte slice to avoid referencing the scanner buffer.
func MakeCopy(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
