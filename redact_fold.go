package rudderstackai

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
	"golang.org/x/text/unicode/norm"
)

// formatCharacters is the Unicode 17.0 Cf set from spec/fixtures/redaction-v0.1.json (a test keeps
// them equal). Hardcoded because Go's unicode.Cf follows the Go release's Unicode version.
var formatCharacters = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00AD, Hi: 0x00AD, Stride: 1},
		{Lo: 0x0600, Hi: 0x0605, Stride: 1},
		{Lo: 0x061C, Hi: 0x061C, Stride: 1},
		{Lo: 0x06DD, Hi: 0x06DD, Stride: 1},
		{Lo: 0x070F, Hi: 0x070F, Stride: 1},
		{Lo: 0x0890, Hi: 0x0891, Stride: 1},
		{Lo: 0x08E2, Hi: 0x08E2, Stride: 1},
		{Lo: 0x180E, Hi: 0x180E, Stride: 1},
		{Lo: 0x200B, Hi: 0x200F, Stride: 1},
		{Lo: 0x202A, Hi: 0x202E, Stride: 1},
		{Lo: 0x2060, Hi: 0x2064, Stride: 1},
		{Lo: 0x2066, Hi: 0x206F, Stride: 1},
		{Lo: 0xFEFF, Hi: 0xFEFF, Stride: 1},
		{Lo: 0xFFF9, Hi: 0xFFFB, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x110BD, Hi: 0x110BD, Stride: 1},
		{Lo: 0x110CD, Hi: 0x110CD, Stride: 1},
		{Lo: 0x13430, Hi: 0x1343F, Stride: 1},
		{Lo: 0x1BCA0, Hi: 0x1BCA3, Stride: 1},
		{Lo: 0x1D173, Hi: 0x1D17A, Stride: 1},
		{Lo: 0xE0001, Hi: 0xE0001, Stride: 1},
		{Lo: 0xE0020, Hi: 0xE007F, Stride: 1},
	},
	LatinOffset: 1,
}

// view is text as the rules see it, one entry per rune: chars[i] came from text[start[i]:end[i]],
// and tok[i] is 0 for the text's own character or the id of the replacement it became part of.
type view struct {
	chars      []rune
	start, end []int32
	tok        []int32
}

// span is a replacement, in byte offsets of the text.
type span struct {
	s, e  int
	token string
}

// isFolded reports that the folded view equals text: ASCII, or no format character and already NFKC.
func isFolded(text string) bool {
	ascii := true
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return true
	}
	for _, c := range text {
		if c >= utf8.RuneSelf && unicode.Is(formatCharacters, c) {
			return false
		}
	}
	return norm.NFKC.IsNormalString(text)
}

func newView(n int) *view {
	return &view{chars: make([]rune, 0, n), start: make([]int32, 0, n), end: make([]int32, 0, n)}
}

func (v *view) add(c rune, s, e int) {
	v.chars = append(v.chars, c)
	v.start = append(v.start, int32(s))
	v.end = append(v.end, int32(e))
}

func ownView(text string) *view {
	v := newView(len(text))
	for i := 0; i < len(text); {
		c, size := utf8.DecodeRuneInString(text[i:])
		v.add(c, i, i+size)
		i += size
	}
	v.tok = make([]int32, len(v.chars))
	return v
}

// foldedView drops format characters and replaces every other code point by its own NFKC form
// (never the whole text's: a character's fold must not depend on its neighbors).
func foldedView(text string) *view {
	v := newView(len(text))
	var buf []byte
	for i := 0; i < len(text); {
		c, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case c < utf8.RuneSelf || (c == utf8.RuneError && size == 1):
			v.add(c, i, i+size)
		case unicode.Is(formatCharacters, c):
		default:
			// A decomposition of one code point is already its NFKC form; the rest go through the
			// allocating call.
			if d := norm.NFKC.PropertiesString(text[i : i+size]).Decomposition(); d == nil {
				v.add(c, i, i+size)
			} else if r, n := utf8.DecodeRune(d); n == len(d) {
				v.add(r, i, i+size)
			} else {
				buf = norm.NFKC.AppendString(buf[:0], text[i:i+size])
				for _, d := range string(buf) {
					v.add(d, i, i+size)
				}
			}
		}
		i += size
	}
	v.tok = make([]int32, len(v.chars))
	return v
}

type cut struct {
	a, b  int
	token string
}

// spans runs the rules over the view as maskPlain does, then returns the replacements still present,
// in text offsets. ok is false when a pattern timed out (reported).
func (v *view) spans(rules []redactRule, compiled []*regexp2.Regexp) (_ []span, ok bool) {
	var tokens []string // token id n is tokens[n-1]
	var cuts []cut
	var spare *view
	for i, r := range rules {
		if len(r.needs) > 0 && !slices.ContainsFunc(r.needs, func(sub string) bool { return hasRunes(v.chars, sub) }) {
			continue
		}
		cuts = cuts[:0]
		m, err := compiled[i].FindRunesMatch(v.chars)
		for ; err == nil && m != nil; m, err = compiled[i].FindNextMatch(m) {
			kept := 0
			if r.keep {
				kept = m.GroupByNumber(1).Length
			}
			a, b := m.Index+kept, m.Index+m.Length
			if r.check != nil && !r.check(string(v.chars[a:b])) {
				continue
			}
			cuts = append(cuts, cut{a, b, "[" + r.token + "]"})
		}
		if err != nil {
			report(KindRedactionTimeout, "redaction pattern "+r.name, err)
			return nil, false
		}
		if len(cuts) == 0 {
			continue
		}
		// Two buffers swap roles, so a rule with matches costs no new allocation.
		next := spare
		if next == nil {
			next = newView(len(v.chars))
			next.tok = make([]int32, 0, len(v.chars))
		}
		next.chars, next.start, next.end, next.tok = next.chars[:0], next.start[:0], next.end[:0], next.tok[:0]
		pos := 0
		for _, c := range cuts {
			next.copyRange(v, pos, c.a)
			tokens = append(tokens, c.token)
			s, e := v.start[c.a], v.end[c.b-1]
			for _, d := range c.token {
				next.chars = append(next.chars, d)
				next.start = append(next.start, s)
				next.end = append(next.end, e)
				next.tok = append(next.tok, int32(len(tokens)))
			}
			pos = c.b
		}
		next.copyRange(v, pos, len(v.chars))
		v, spare = next, v
	}
	var out []span
	for i, t := range v.tok {
		if t != 0 && (i == 0 || v.tok[i-1] != t) {
			out = append(out, span{int(v.start[i]), int(v.end[i]), tokens[t-1]})
		}
	}
	return out, true
}

func hasRunes(chars []rune, sub string) bool {
	first, size := utf8.DecodeRuneInString(sub)
	for i, c := range chars {
		if c != first {
			continue
		}
		j := i + 1
		for _, r := range sub[size:] {
			if j >= len(chars) || chars[j] != r {
				j = -1
				break
			}
			j++
		}
		if j >= 0 {
			return true
		}
	}
	return false
}

func (v *view) copyRange(from *view, a, b int) {
	v.chars = append(v.chars, from.chars[a:b]...)
	v.start = append(v.start, from.start[a:b]...)
	v.end = append(v.end, from.end[a:b]...)
	v.tok = append(v.tok, from.tok[a:b]...)
}

// mergeSpans unions the own view's replacements with the folded view's: overlapping ones become one
// replacement that takes the token of the first in (start, longest, own view) order.
func mergeSpans(own, folded []span) []span {
	type part struct {
		span
		folded bool
	}
	parts := make([]part, 0, len(own)+len(folded))
	for _, s := range own {
		parts = append(parts, part{s, false})
	}
	for _, s := range folded {
		parts = append(parts, part{s, true})
	}
	slices.SortStableFunc(parts, func(x, y part) int {
		switch {
		case x.s != y.s:
			return x.s - y.s
		case x.e != y.e:
			return y.e - x.e
		case x.folded != y.folded && !x.folded:
			return -1
		case x.folded != y.folded:
			return 1
		}
		return 0
	})
	var merged []span
	for _, p := range parts {
		if n := len(merged); n > 0 && p.s < merged[n-1].e {
			merged[n-1].e = max(merged[n-1].e, p.e)
		} else {
			merged = append(merged, p.span)
		}
	}
	return merged
}

func applySpans(text string, spans []span) string {
	var b strings.Builder
	b.Grow(len(text))
	cursor := 0
	for _, s := range spans {
		b.WriteString(text[cursor:s.s])
		b.WriteString(s.token)
		cursor = s.e
	}
	b.WriteString(text[cursor:])
	return b.String()
}
