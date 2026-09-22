// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import "fmt"

// stripJSONC turns JSON-with-comments into JSON encoding/json accepts, in one
// pass tracking string and escape state. Line (//) and block (/* */) comments
// are blanked with spaces and a comma whose next significant byte is } or ]
// is blanked too, so the output always has the input's length and every byte
// offset encoding/json reports points into the original file. Newlines inside
// comments are kept. Comment markers inside strings are untouched. An
// unterminated string or block comment is an error.
func stripJSONC(in []byte) ([]byte, error) {
	out := make([]byte, len(in))
	copy(out, in)
	for i := 0; i < len(in); {
		switch {
		case in[i] == '"':
			end, ok := stringEnd(in, i)
			if !ok {
				return nil, fmt.Errorf("unterminated string at offset %d", i)
			}
			i = end
		case in[i] == '/' && i+1 < len(in) && in[i+1] == '/':
			for i < len(in) && in[i] != '\n' {
				out[i] = ' '
				i++
			}
		case in[i] == '/' && i+1 < len(in) && in[i+1] == '*':
			end, ok := blockCommentEnd(in, i)
			if !ok {
				return nil, fmt.Errorf("unterminated block comment at offset %d", i)
			}
			for j := i; j < end; j++ {
				if out[j] != '\n' {
					out[j] = ' '
				}
			}
			i = end
		case in[i] == ',':
			if next := nextSignificant(in, i+1); next < len(in) && (in[next] == '}' || in[next] == ']') {
				out[i] = ' '
			}
			i++
		default:
			i++
		}
	}
	return out, nil
}

// stringEnd returns the offset just past the string opening at in[i], or
// false when it never closes.
func stringEnd(in []byte, i int) (int, bool) {
	for j := i + 1; j < len(in); j++ {
		switch in[j] {
		case '\\':
			j++
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

// blockCommentEnd returns the offset just past the block comment opening at
// in[i], or false when it never closes.
func blockCommentEnd(in []byte, i int) (int, bool) {
	for j := i + 2; j+1 < len(in); j++ {
		if in[j] == '*' && in[j+1] == '/' {
			return j + 2, true
		}
	}
	return 0, false
}

// nextSignificant returns the offset of the first byte at or after i that is
// neither whitespace nor inside a comment, or len(in) when there is none. An
// unterminated block comment counts as reaching the end; the main pass
// reports it.
func nextSignificant(in []byte, i int) int {
	for i < len(in) {
		switch {
		case in[i] == ' ' || in[i] == '\t' || in[i] == '\n' || in[i] == '\r':
			i++
		case in[i] == '/' && i+1 < len(in) && in[i+1] == '/':
			for i < len(in) && in[i] != '\n' {
				i++
			}
		case in[i] == '/' && i+1 < len(in) && in[i+1] == '*':
			end, ok := blockCommentEnd(in, i)
			if !ok {
				return len(in)
			}
			i = end
		default:
			return i
		}
	}
	return len(in)
}
