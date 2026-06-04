// ABOUTME: Shared utility functions for source adapters.
// ABOUTME: Provides common helpers like byte slice copying and unbounded JSONL line reads.

package adapter

import "bufio"

// MakeCopy returns a copy of the byte slice to avoid referencing the scanner buffer.
func MakeCopy(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// ReadLine returns the next line from r with any trailing CR/LF stripped. A non-nil
// error (including io.EOF) may be paired with a non-empty final line when the file
// does not end with a newline. Unlike bufio.Scanner, there is no token size cap, so
// arbitrarily large JSONL lines (e.g. embedded tool output) are handled.
func ReadLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
	}
	return line, err
}
