package chunk

import (
	"reflect"
	"testing"
)

func TestPythonStrip(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ascii whitespace", "  \t hi \n", "hi"},
		{"c0 separators", "\x1c\x1dhi\x1e\x1f", "hi"},
		{"nbsp", " hi ", "hi"},
		{"all whitespace", " \t\n\x1c ", ""},
		{"interior kept", "a b", "a b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pythonStrip(tt.in); got != tt.want {
				t.Errorf("pythonStrip(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecodeReplace(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"valid ascii", []byte("hello"), "hello"},
		{"valid multibyte", []byte("café"), "café"},
		{"stray byte", []byte{'a', 0xff, 'b'}, "a�b"},
		{"leading invalid", []byte{0x80, 'x'}, "�x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeReplace(tt.in); got != tt.want {
				t.Errorf("decodeReplace(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitLineSpans(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []boundary
	}{
		{
			name: "lf cr crlf vt mix",
			src:  "a\nb\rc\r\nd\x0be",
			want: []boundary{{0, 2}, {2, 4}, {4, 7}, {7, 9}, {9, 10}},
		},
		{
			name: "multibyte line separator",
			src:  "a b",
			want: []boundary{{0, 4}, {4, 5}},
		},
		{
			name: "trailing newline has no empty line",
			src:  "x\n",
			want: []boundary{{0, 2}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitLineSpans([]byte(tt.src))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitLineSpans(%q) = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}

func TestCountNewlines(t *testing.T) {
	src := []byte("a\nb\nc")
	tests := []struct {
		end  int
		want int
	}{
		{0, 0}, {2, 1}, {4, 2}, {5, 2},
	}
	for _, tt := range tests {
		if got := countNewlines(src, tt.end); got != tt.want {
			t.Errorf("countNewlines(%q[:%d]) = %d, want %d", src, tt.end, got, tt.want)
		}
	}
}
