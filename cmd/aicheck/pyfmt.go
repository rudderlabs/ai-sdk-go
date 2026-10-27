package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Values print as Python prints them, so the three checkers share their output byte for byte.

// repr is Python's repr() of a JSON value.
func repr(v any) string {
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

func writeRepr(b *strings.Builder, v any) {
	switch v := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if v {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case Int:
		b.WriteString(string(v))
	case Float:
		b.WriteString(floatRepr(v))
	case string:
		b.WriteString(reprStr(v))
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, item)
		}
		b.WriteByte(']')
	case []string:
		b.WriteString(reprList(v))
	case *Object:
		b.WriteByte('{')
		for i, k := range v.keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(reprStr(k))
			b.WriteString(": ")
			writeRepr(b, v.vals[k])
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("aicheck: no repr for %T", v))
	}
}

// reprList is the repr of a list of strings.
func reprList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = reprStr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// text is a plain value as an f-string interpolates it: a string as is, anything else as repr.
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return repr(v)
}

// floatRepr is Python's repr() of a float: the shortest digits that round-trip, in exponent form
// below 1e-4 or from 1e16, with ".0" on integral values.
func floatRepr(f Float) string {
	x := float64(f)
	switch {
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	case math.IsNaN(x):
		return "nan"
	}
	if a := math.Abs(x); a != 0 && (a < 1e-4 || a >= 1e16) {
		return strconv.FormatFloat(x, 'e', -1, 64) // 1e-05, 1.5e+16: Go pads the exponent as Python does
	}
	s := strconv.FormatFloat(x, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// isPrintable is Python's str.isprintable() for one character: letters, marks, numbers,
// punctuation, symbols and the ASCII space.
func isPrintable(r rune) bool {
	if r == ' ' {
		return true
	}
	if r >= 0xd800 && r <= 0xdfff {
		return false
	}
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S)
}

// reprStr is Python's repr() of a str.
func reprStr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, n := decodeRune(s[i:])
		i += n
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case isPrintable(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// dumps is Python's json.dumps(v, sort_keys=True), with indent=2 when indent is set.
func dumps(v any, indent bool) string {
	var b strings.Builder
	writeJSON(&b, v, indent, 0)
	return b.String()
}

func newline(b *strings.Builder, indent bool, depth int) {
	if indent {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat("  ", depth))
	}
}

func writeJSON(b *strings.Builder, v any, indent bool, depth int) {
	sep := ", "
	if indent {
		sep = ","
	}
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case Int:
		b.WriteString(string(v))
	case Float:
		switch x := float64(v); {
		case math.IsInf(x, 1):
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			b.WriteString("-Infinity")
		case math.IsNaN(x):
			b.WriteString("NaN")
		default:
			b.WriteString(floatRepr(v))
		}
	case string:
		writeJSONStr(b, v)
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteString(sep)
			}
			newline(b, indent, depth+1)
			writeJSON(b, item, indent, depth+1)
		}
		newline(b, indent, depth)
		b.WriteByte(']')
	case *Object:
		if v.Len() == 0 {
			b.WriteString("{}")
			return
		}
		keys := append([]string(nil), v.keys...)
		sort.Strings(keys) // WTF-8 byte order is code point order
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(sep)
			}
			newline(b, indent, depth+1)
			writeJSONStr(b, k)
			b.WriteString(": ")
			writeJSON(b, v.vals[k], indent, depth+1)
		}
		newline(b, indent, depth)
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("aicheck: no JSON for %T", v))
	}
}

// writeJSONStr writes a JSON string with ensure_ascii: everything outside printable ASCII escaped.
func writeJSONStr(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, n := decodeRune(s[i:])
		i += n
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= ' ' && r < 0x7f:
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
}
