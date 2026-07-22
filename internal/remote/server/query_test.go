// ABOUTME: Unit tests for the shell-style tokenizer that backs parseKV
// ABOUTME: Guards against strings.Fields regressions on multi-word remote search

package server

import (
	"reflect"
	"testing"
)

func TestParseKVQuoting(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "bare word",
			in:   "q=word",
			want: map[string]string{"q": "word"},
		},
		{
			name: "quoted multi-word",
			in:   `q="two words"`,
			want: map[string]string{"q": "two words"},
		},
		{
			name: "quoted plus bare",
			in:   `q="two words" project=foo`,
			want: map[string]string{"q": "two words", "project": "foo"},
		},
		{
			name: "escaped nested quotes",
			in:   `q="has \"nested\" quotes"`,
			want: map[string]string{"q": `has "nested" quotes`},
		},
		{
			name: "extra whitespace tolerated",
			in:   "  q=fox    project=foo  ",
			want: map[string]string{"q": "fox", "project": "foo"},
		},
		{
			name: "tabs between args",
			in:   "q=fox\tproject=foo",
			want: map[string]string{"q": "fox", "project": "foo"},
		},
		{
			name: "empty input",
			in:   "",
			want: map[string]string{},
		},
		{
			name: "quoted value containing equals",
			in:   `q="a=b c=d"`,
			want: map[string]string{"q": "a=b c=d"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseKV(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseKV(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
