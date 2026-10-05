// ABOUTME: The storage policy for tool payloads — what gets stored whole, what is reduced to a length.
// ABOUTME: Shared by pkg/parser and every adapter so all five sources classify a result the same way.

// Package toolpayload decides what ccvault keeps of a tool call's input and
// result. It exists as its own package because the decision has to be the same
// for all five sources, and the code that extracts them is split between
// pkg/parser (Claude Code and nanoclaw) and the individual adapters (codex,
// jeff) with no shared layer below both.
//
// The policy, settled on issue #28 from measurement rather than preference:
//
//   - Tool inputs are stored whole and indexed. Across 259,836 calls in the
//     author's archive they total 78.6 MB, p50 72 bytes, largest 99,792.
//   - Tool results are stored whole and indexed, except where the content is
//     bulk or unindexable. Across 211,892 such results they total 397 MB,
//     p50 525 bytes, largest 51,557.
//   - Bulk file reads and image payloads keep only their length. Their text is
//     still in turns.raw_json, and the row carries file_path already, so
//     nothing is lost — but 205 MB of file dumps and 55 MB of base64 in the FTS
//     index would wreck relevance for the shell commands and error messages
//     this material exists to make searchable.
//
// Nothing is ever stored truncated to a prefix. raw_json keeps the full
// original regardless, so a prefix costs storage without preserving anything:
// the choices are store-whole and store-metadata-only.
package toolpayload

import (
	"encoding/json"
	"strings"
)

// MaxStoredResultBytes is a backstop, not a cap in the ordinary sense. Every
// result class that exists today is classified by shape or by tool, and the
// largest non-bulk result measured across 211,892 of them is 51,557 bytes —
// so this fires on nothing in the archive. It is here for the tool nobody has
// classified yet: a future Bash wrapper that returns a whole file, an MCP
// server that answers with a megabyte of JSON. Uncapped in principle, bounded
// in practice.
const MaxStoredResultBytes = 128 * 1024

// Reasons a result's content was left out. The empty string means it was
// stored. A consumer reads these rather than re-deriving the classification
// from tool_name and result_length, which is the guessing issue #28 set out to
// remove.
const (
	// OmitBulkRead: a bulk file reader. The row's file_path says what was
	// read and result_length says how much came back.
	OmitBulkRead = "bulk_read"
	// OmitImage: the content carried an image block. Detected by shape, so
	// it catches MCP tools whose names say nothing about images.
	OmitImage = "image"
	// OmitOversize: over MaxStoredResultBytes.
	OmitOversize = "oversize"
	// OmitUndecodable: the content was not a shape this package knows how to
	// read. The length is still recorded; raw_json still has the original.
	OmitUndecodable = "undecodable"
)

// bulkReadTools are the tools whose results are file contents rather than
// anything a search over tool output should rank. Matched by name, unlike
// images: these are a fixed, known set of Claude Code tools, and the backstop
// above covers the unknown ones.
var bulkReadTools = map[string]bool{
	"Read":         true,
	"NotebookRead": true,
}

// IsBulkReadTool reports whether a tool's results are bulk file contents.
func IsBulkReadTool(toolName string) bool {
	return bulkReadTools[toolName]
}

// Result is the decision for one tool result.
type Result struct {
	// Content is the result text to store, empty when the content was left
	// out. Also empty, legitimately, when the tool returned nothing — read
	// OmitReason to tell those apart.
	Content string

	// Length is the size of the result as the transcript carries it: the
	// string's bytes for a string content, the JSON encoding's bytes for a
	// structured one. Recorded whether or not Content was stored, which is
	// what lets a consumer distinguish a short result from an omitted one.
	//
	// For a structured content this runs ahead of len(Content) by the JSON
	// framing — about 19% across the archive's 130,516 array-valued results.
	// It is deliberately the transcript's number rather than the extraction's,
	// because an image result extracts to no text at all and a zero there
	// would destroy the only signal the row has left.
	Length int

	// OmitReason names why Content was left out, empty when it was stored.
	OmitReason string
}

// resultContentBlock is one element of a structured tool_result content array.
type resultContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResultFromJSON applies the policy to a Claude Code tool_result content
// field, which the transcripts write either as a JSON string or as an array of
// content blocks.
func ResultFromJSON(toolName string, rawContent json.RawMessage) Result {
	trimmed := strings.TrimSpace(string(rawContent))
	if trimmed == "" || trimmed == "null" {
		return Result{}
	}

	// A JSON string: the length is the string's own bytes, so a stored result
	// has Length == len(Content).
	var text string
	if err := json.Unmarshal(rawContent, &text); err == nil {
		return decide(toolName, text, len(text), false)
	}

	// An array of content blocks. Length is the array's JSON bytes — see the
	// Result.Length comment for why the transcript's number is the right one.
	var blocks []resultContentBlock
	if err := json.Unmarshal(rawContent, &blocks); err == nil {
		hasImage := false
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "image":
				hasImage = true
			case "text":
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
			}
		}
		return decide(toolName, strings.Join(parts, "\n"), len(rawContent), hasImage)
	}

	return Result{Length: len(rawContent), OmitReason: OmitUndecodable}
}

// ResultFromText applies the policy to a tool result the source records as a
// bare string — codex writes function_call_output.output that way, jeff writes
// tool_result.output_preview. No image shape is reachable here, so only the
// bulk-tool and backstop rules can fire.
func ResultFromText(toolName, text string) Result {
	return decide(toolName, text, len(text), false)
}

// decide is the policy itself, with the shapes already resolved. Image first:
// it is the shape-based rule and the most specific thing true about a result
// that carries one. Then the bulk tools, then the backstop.
func decide(toolName, content string, length int, hasImage bool) Result {
	switch {
	case hasImage:
		return Result{Length: length, OmitReason: OmitImage}
	case IsBulkReadTool(toolName):
		return Result{Length: length, OmitReason: OmitBulkRead}
	case length > MaxStoredResultBytes:
		return Result{Length: length, OmitReason: OmitOversize}
	default:
		return Result{Content: content, Length: length}
	}
}
