package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The JSON values the checker works on, decoded as Python's json module does, so every checker
// prints them alike: nil, bool, Int, Float, string, []any and *Object. Strings are WTF-8: a lone
// surrogate escape (\ud800) keeps its code point, encoded like a UTF-8 three-byte sequence.

// Int is a JSON integer literal (no '.', 'e' or 'E'), exact however large, in canonical form.
type Int string

// Float is any other JSON number.
type Float float64

// Object is a JSON object that keeps its key order. A repeated key keeps its first position and
// its last value.
type Object struct {
	keys []string
	vals map[string]any
}

func newObject() *Object { return &Object{vals: map[string]any{}} }

// Set adds or replaces a key.
func (o *Object) Set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// Get returns the value of k, nil when absent.
func (o *Object) Get(k string) any { return o.vals[k] }

// Lookup returns the value of k and whether it is present.
func (o *Object) Lookup(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// Has reports whether k is present.
func (o *Object) Has(k string) bool {
	_, ok := o.vals[k]
	return ok
}

// Keys returns the keys in order.
func (o *Object) Keys() []string { return o.keys }

// Len is the number of keys.
func (o *Object) Len() int { return len(o.keys) }

// errCapture is a capture or schema the checker cannot use: exit code 2.
type errCapture struct{ msg string }

func (e *errCapture) Error() string { return e.msg }

func captureErr(format string, args ...any) error {
	return &errCapture{fmt.Sprintf(format, args...)}
}

var lineBreak = regexp.MustCompile(`\r\n|\r|\n`)

// load returns every JSON object in a JSONL file, in order; blank lines are skipped.
func load(path string) ([]*Object, error) {
	b, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(b) {
		return nil, captureErr("%s: cannot read file", path)
	}
	var messages []*Object
	for i, line := range lineBreak.Split(string(b), -1) {
		if strings.Trim(line, " \t\f\v") == "" {
			continue
		}
		v, err := parseJSON(line)
		if err != nil {
			return nil, captureErr("%s:%d: not valid JSON", path, i+1)
		}
		m, ok := v.(*Object)
		if !ok {
			return nil, captureErr("%s:%d: not a JSON object", path, i+1)
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// obj is the value if it is a JSON object, else an empty one: malformed fields read as absent.
func obj(v any) *Object {
	if o, ok := v.(*Object); ok {
		return o
	}
	return newObject()
}

// tracks returns the track calls with an event name; messages without type are track calls.
func tracks(messages []*Object) []*Object {
	var out []*Object
	for _, m := range messages {
		t, hasType := m.Lookup("type")
		if hasType && t != "track" {
			continue
		}
		if e, ok := m.Get("event").(string); ok && e != "" {
			out = append(out, m)
		}
	}
	return out
}

// truthy is Python's truth value of a JSON value.
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case Int:
		return v != "0"
	case Float:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case *Object:
		return v.Len() > 0
	}
	return true
}

// key returns v when it is a non-empty string: the only IDs that join.
func key(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && s != ""
}

// --- decoder: Python's json.loads, strict, NaN and Infinity rejected ---

var errSyntax = errors.New("invalid JSON")

// maxIntDigits is Python's default limit on int() of a decimal string (sys.int_info).
const maxIntDigits = 4300

type parser struct {
	s string
	i int
}

func parseJSON(s string) (any, error) {
	p := &parser{s: s}
	p.ws()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, errSyntax
	}
	return v, nil
}

func (p *parser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) literal(word string, v any) (any, error) {
	if !strings.HasPrefix(p.s[p.i:], word) {
		return nil, errSyntax
	}
	p.i += len(word)
	return v, nil
}

func (p *parser) value() (any, error) {
	if p.i >= len(p.s) {
		return nil, errSyntax
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == 't':
		return p.literal("true", true)
	case c == 'f':
		return p.literal("false", false)
	case c == 'n':
		return p.literal("null", nil)
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, errSyntax
}

func (p *parser) object() (any, error) {
	p.i++ // {
	o := newObject()
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return o, nil
	}
	for {
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, errSyntax
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, errSyntax
		}
		p.i++
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		o.Set(k.(string), v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, errSyntax
		}
		switch p.s[p.i] {
		case ',':
			p.i++
			p.ws()
		case '}':
			p.i++
			return o, nil
		default:
			return nil, errSyntax
		}
	}
}

func (p *parser) array() (any, error) {
	p.i++ // [
	out := []any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, errSyntax
		}
		switch p.s[p.i] {
		case ',':
			p.i++
			p.ws()
		case ']':
			p.i++
			return out, nil
		default:
			return nil, errSyntax
		}
	}
}

func (p *parser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
	if err != nil {
		return 0, false
	}
	p.i += 4
	return rune(n), true
}

func (p *parser) str() (any, error) {
	p.i++ // "
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return nil, errSyntax
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c < 0x20:
			return nil, errSyntax
		case c != '\\':
			b.WriteByte(c)
			p.i++
			continue
		}
		p.i++ // backslash
		if p.i >= len(p.s) {
			return nil, errSyntax
		}
		e := p.s[p.i]
		p.i++
		switch e {
		case '"', '\\', '/':
			b.WriteByte(e)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return nil, errSyntax
			}
			// A high surrogate followed by an escaped low one is one code point; any other stays.
			if r >= 0xd800 && r <= 0xdbff && strings.HasPrefix(p.s[p.i:], `\u`) {
				save := p.i
				p.i += 2
				if lo, ok := p.hex4(); ok && lo >= 0xdc00 && lo <= 0xdfff {
					r = 0x10000 + (r-0xd800)<<10 + (lo - 0xdc00)
				} else {
					p.i = save
				}
			}
			writeRune(&b, r)
		default:
			return nil, errSyntax
		}
	}
}

func (p *parser) digits() int {
	start := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	return p.i - start
}

func (p *parser) number() (any, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	if p.i < len(p.s) && p.s[p.i] == '0' {
		p.i++
	} else if p.digits() == 0 {
		return nil, errSyntax
	}
	isFloat := false
	if p.i < len(p.s) && p.s[p.i] == '.' {
		save := p.i
		p.i++
		if p.digits() == 0 {
			p.i = save // "1." is the integer 1 followed by junk
		} else {
			isFloat = true
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		save := p.i
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if p.digits() == 0 {
			p.i = save
		} else {
			isFloat = true
		}
	}
	lit := p.s[start:p.i]
	if !isFloat {
		if len(strings.TrimPrefix(lit, "-")) > maxIntDigits {
			return nil, errSyntax
		}
		if lit == "-0" {
			lit = "0"
		}
		return Int(lit), nil
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, errSyntax
	}
	return Float(f), nil // out of range: ±inf, as Python's float()
}

// --- WTF-8: strings that may hold lone surrogates ---

func writeRune(b *strings.Builder, r rune) {
	if r >= 0xd800 && r <= 0xdfff {
		b.WriteByte(0xed)
		b.WriteByte(byte(0x80 | (r>>6)&0x3f))
		b.WriteByte(byte(0x80 | r&0x3f))
		return
	}
	b.WriteRune(r)
}

// decodeRune is utf8.DecodeRuneInString that also reads an encoded surrogate.
func decodeRune(s string) (rune, int) {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && n == 1 && len(s) >= 3 && s[0] == 0xed && s[1] >= 0xa0 && s[1] <= 0xbf && s[2]&0xc0 == 0x80 {
		return 0xd000 | rune(s[1]&0x3f)<<6 | rune(s[2]&0x3f), 3
	}
	return r, n
}

func isFinite(f Float) bool { return !math.IsInf(float64(f), 0) && !math.IsNaN(float64(f)) }
