// ABOUTME: Tests for the shared adapter utility helpers.
// ABOUTME: Exercises ReadLine's no-cap behaviour and trailing-line edge cases.

package adapter

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadLine_LargeLine(t *testing.T) {
	// bufio.Scanner with default settings caps tokens at 64KB; the previous
	// codex/jeff parsers raised that to 1MiB but still failed on bigger lines
	// (e.g. Codex tool output). ReadLine must handle lines well beyond 1MiB.
	const sz = 4 * 1024 * 1024 // 4MiB
	payload := strings.Repeat("x", sz)
	input := payload + "\n" + "next-line\n"

	r := bufio.NewReader(strings.NewReader(input))

	line, err := ReadLine(r)
	if err != nil {
		t.Fatalf("ReadLine err: %v", err)
	}
	if len(line) != sz {
		t.Fatalf("first line length = %d, want %d", len(line), sz)
	}
	if !bytes.Equal(line, []byte(payload)) {
		t.Fatal("first line content mismatch")
	}

	line, err = ReadLine(r)
	if err != nil {
		t.Fatalf("ReadLine err: %v", err)
	}
	if string(line) != "next-line" {
		t.Errorf("second line = %q, want %q", string(line), "next-line")
	}
}

func TestReadLine_NoTrailingNewline(t *testing.T) {
	// A line not terminated by \n must still be returned, paired with io.EOF.
	r := bufio.NewReader(strings.NewReader("only-line"))

	line, err := ReadLine(r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if string(line) != "only-line" {
		t.Errorf("line = %q, want %q", string(line), "only-line")
	}
}

func TestReadLine_StripsCRLF(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("windows\r\nunix\n"))

	line, err := ReadLine(r)
	if err != nil {
		t.Fatalf("ReadLine err: %v", err)
	}
	if string(line) != "windows" {
		t.Errorf("first line = %q, want %q", string(line), "windows")
	}

	line, err = ReadLine(r)
	if err != nil {
		t.Fatalf("ReadLine err: %v", err)
	}
	if string(line) != "unix" {
		t.Errorf("second line = %q, want %q", string(line), "unix")
	}
}

func TestReadLine_EmptyReader(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))

	line, err := ReadLine(r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(line) != 0 {
		t.Errorf("line = %q, want empty", string(line))
	}
}
