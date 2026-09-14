// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package grpc

import "testing"

func TestStripTerminalQueryResponses_EOLMark(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "zsh default PROMPT_EOL_MARK (bold+standout %, padded, bare CR)",
			in:   "hello\x1b[1m\x1b[7m%\x1b[27m\x1b[m    \r[~]$ ",
			want: "hello[~]$ ",
		},
		{
			name: "single SGR wrap with # marker (root prompt)",
			in:   "out\x1b[7m#\x1b[27m  \r$ ",
			want: "out$ ",
		},
		{
			name: "no padding spaces still strips",
			in:   "x\x1b[7m%\x1b[27m\r$ ",
			want: "x$ ",
		},
		{
			name: "CRLF is left untouched (normal line ending, not the EOL mark)",
			in:   "line one\r\nline two\r\n",
			want: "line one\r\nline two\r\n",
		},
		{
			name: "bare % without SGR wrapping is not stripped (e.g. a real progress indicator)",
			in:   "50%   \rdone",
			want: "50%   \rdone",
		},
		{
			name: "styled text without the marker char is left alone",
			in:   "\x1b[1mBOLD\x1b[0m text",
			want: "\x1b[1mBOLD\x1b[0m text",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(stripTerminalQueryResponses([]byte(c.in)))
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestStripTerminalQueryResponses_ExistingCSIFiltering(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "CPR response stripped",
			in:   "before\x1b[24;80Rafter",
			want: "beforeafter",
		},
		{
			name: "primary DA response stripped",
			in:   "before\x1b[?1;2cafter",
			want: "beforeafter",
		},
		{
			name: "OSC 11 colour response stripped (BEL terminated)",
			in:   "before\x1b]11;rgb:0000/0000/0000\x07after",
			want: "beforeafter",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(stripTerminalQueryResponses([]byte(c.in)))
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
