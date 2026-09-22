// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestStripJSONC(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"identity", `{"a": [1, 2], "b": {"c": "d"}}`, `{"a": [1, 2], "b": {"c": "d"}}`},
		{"empty input", "", ""},
		{"line comment before", "// top\n{\"a\": 1}", "      \n{\"a\": 1}"},
		{"line comment between", "{\"a\": // why\n 1}", "{\"a\":       \n 1}"},
		{"line comment at end without newline", `{"a": 1} // done`, `{"a": 1}        `},
		{"block comment before", `/* x */{"a": 1}`, `       {"a": 1}`},
		{"block comment between key and value", `{"a": /* x */ 1}`, `{"a":         1}`},
		{"block comment after", `{"a": 1}/**/`, `{"a": 1}    `},
		{"multi-line block comment keeps newlines", "{/* a\nb\n*/\"a\": 1}", "{    \n \n  \"a\": 1}"},
		{"trailing comma in object", `{"a": 1,}`, `{"a": 1 }`},
		{"trailing comma in array", `[1, 2,]`, `[1, 2 ]`},
		{"trailing comma with whitespace", "{\"a\": 1,\n}", "{\"a\": 1 \n}"},
		{"trailing comma before a line comment", "{\"a\": 1, // c\n}", "{\"a\": 1      \n}"},
		{"trailing comma before a block comment", `[1, /* c */ ]`, `[1          ]`},
		{"nested trailing commas", `{"a": [1,], "b": {"c": 2,},}`, `{"a": [1 ], "b": {"c": 2 } }`},
		{"comma in an empty object", `{,}`, `{ }`},
		{"non-trailing comma stays", `[1,2]`, `[1,2]`},
		{"comment markers inside strings survive", `{"a": "// not", "b": "/* nor */"}`,
			`{"a": "// not", "b": "/* nor */"}`},
		{"escaped quote then marker outside", `{"a": "x\" y" // c` + "\n}", `{"a": "x\" y"     ` + "\n}"},
		{"escaped backslash closes the string", `{"a": "x\\"} // c`, `{"a": "x\\"}     `},
		{"a lone slash is not a comment", `{"a": 1}/`, `{"a": 1}/`},
		{"comma inside a string is not trailing", `{"a": ","}`, `{"a": ","}`},
		{"crlf line comment", "{\"a\": 1 // c\r\n}", "{\"a\": 1      \n}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := stripJSONC([]byte(c.in))
			if err != nil {
				t.Fatalf("stripJSONC(%q): %v", c.in, err)
			}
			if string(got) != c.want {
				t.Errorf("stripJSONC(%q) =\n  %q\nwant\n  %q", c.in, got, c.want)
			}
			if len(got) != len(c.in) {
				t.Errorf("len = %d, want %d", len(got), len(c.in))
			}
		})
	}
}

func TestStripJSONCErrors(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"unterminated block comment", `{"a": 1 /* never`, "unterminated block comment at offset 8"},
		{"unterminated block comment at end", `{} /*`, "unterminated block comment at offset 3"},
		{"unterminated string", `{"a": "open`, "unterminated string at offset 6"},
		{"string ending in a backslash", `{"a": "x\`, "unterminated string at offset 6"},
		{"comment opener inside unterminated string", `{"a": "/*`, "unterminated string at offset 6"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := stripJSONC([]byte(c.in))
			if err == nil || err.Error() != c.want {
				t.Errorf("stripJSONC(%q) error = %v, want %q", c.in, err, c.want)
			}
		})
	}
}

// stringPieces are the fragments generated strings are built from: every
// marker the pre-processor cares about, plus escapes and non-ASCII.
var stringPieces = []string{
	"a", "b c", "//", "/*", "*/", "\"", "\\", "{", "}", "[", "]", ",", ":", "\n", "\t", "é", "😀", "$",
}

func genString(r *rand.Rand) string {
	var sb strings.Builder
	for n := r.Intn(5); n > 0; n-- {
		sb.WriteString(stringPieces[r.Intn(len(stringPieces))])
	}
	return sb.String()
}

func genValue(r *rand.Rand, depth int) any {
	kinds := 6
	if depth <= 0 {
		kinds = 4
	}
	switch r.Intn(kinds) {
	case 0:
		return nil
	case 1:
		return r.Intn(2) == 0
	case 2:
		return float64(r.Intn(100000)) / 8
	case 3:
		return genString(r)
	case 4:
		n := r.Intn(4)
		out := make([]any, n)
		for i := range out {
			out[i] = genValue(r, depth-1)
		}
		return out
	default:
		out := map[string]any{}
		for n := r.Intn(4); n > 0; n-- {
			out[genString(r)] = genValue(r, depth-1)
		}
		return out
	}
}

func marshal(t *testing.T, v any, indent bool) []byte {
	t.Helper()
	var b []byte
	var err error
	if indent {
		b, err = json.MarshalIndent(v, "", "  ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStripJSONCIdentityProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20260922))
	for i := 0; i < 500; i++ {
		v := genValue(r, 4)
		in := marshal(t, v, i%2 == 0)
		got, err := stripJSONC(in)
		if err != nil {
			t.Fatalf("case %d: stripJSONC(%s): %v", i, in, err)
		}
		if string(got) != string(in) {
			t.Fatalf("case %d: not the identity on comment-free JSON:\n in  %s\n out %s", i, in, got)
		}
	}
}

var commentBodies = []string{"c", "* /", "//", "/*", `"quoted"`, "{,]", "é", ""}

// inject rewrites JSON into JSONC: at every whitespace run and before every
// closing bracket outside strings it may add line or block comments, and
// before a closer it may add a trailing comma. The value is unchanged.
func inject(r *rand.Rand, in []byte) []byte {
	var out []byte
	for i := 0; i < len(in); i++ {
		if in[i] == '"' {
			end, _ := stringEnd(in, i)
			out = append(out, in[i:end]...)
			i = end - 1
			continue
		}
		if in[i] == '}' || in[i] == ']' {
			if r.Intn(2) == 0 {
				out = append(out, ',')
				out = append(out, comment(r)...)
			}
		}
		if in[i] == ' ' || in[i] == '\n' {
			if r.Intn(3) == 0 {
				out = append(out, comment(r)...)
			}
		}
		out = append(out, in[i])
	}
	if r.Intn(2) == 0 {
		out = append(out, comment(r)...)
	}
	return out
}

func comment(r *rand.Rand) string {
	body := commentBodies[r.Intn(len(commentBodies))]
	if r.Intn(2) == 0 {
		return "/*" + body + "*/"
	}
	return "//" + body + "\n"
}

func TestStripJSONCInjectedProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20260922))
	for i := 0; i < 500; i++ {
		v := genValue(r, 4)
		in := inject(r, marshal(t, v, i%2 == 0))
		got, err := stripJSONC(in)
		if err != nil {
			t.Fatalf("case %d: stripJSONC(%s): %v", i, in, err)
		}
		if len(got) != len(in) {
			t.Fatalf("case %d: len %d, want %d", i, len(got), len(in))
		}
		var back any
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("case %d: decode stripped output %s: %v", i, got, err)
		}
		if !reflect.DeepEqual(back, v) {
			t.Fatalf("case %d: decoded %#v, want %#v\n in  %s\n out %s", i, back, v, in, got)
		}
	}
}

func TestStripJSONCLengthProperty(t *testing.T) {
	// Any input at all, including garbage, keeps its length or errors.
	r := rand.New(rand.NewSource(1))
	alphabet := []byte("{}[],:\"\\/* \n\tab1")
	for i := 0; i < 2000; i++ {
		in := make([]byte, r.Intn(24))
		for j := range in {
			in[j] = alphabet[r.Intn(len(alphabet))]
		}
		got, err := stripJSONC(in)
		if err != nil {
			continue
		}
		if len(got) != len(in) {
			t.Fatalf("stripJSONC(%q) len %d, want %d", in, len(got), len(in))
		}
	}
}
