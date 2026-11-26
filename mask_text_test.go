package rudderstackai

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func TestMaskTextCodePointBoundary(t *testing.T) {
	const sensitive = " ann@example.com"
	prefix := strings.Repeat("😀", 32_000-utf8.RuneCountInString(sensitive))
	if got, ok := MaskText(prefix + sensitive); !ok || got != prefix+" [email]" {
		t.Errorf("32,000 code points: ok=%t, masked correctly=%t", ok, got == prefix+" [email]")
	}
	if got, ok := MaskText(prefix + sensitive + "😀"); ok || got != "" {
		t.Errorf("32,001 code points: ok=%t, empty=%t", ok, got == "")
	}
}

func TestMaskTextIgnoresSettings(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "redaction_off"
		if enabled {
			name = "redaction_on"
		}
		t.Run(name, func(t *testing.T) {
			setupAI(t, WithRedaction(enabled), WithRedactHook(func(string) (string, error) {
				t.Error("MaskText called the configured redact hook")
				return "hook result", nil
			}))
			if got, ok := MaskText("mail ann@example.com"); !ok || got != "mail [email]" {
				t.Errorf("MaskText = %q, %t", got, ok)
			}
		})
	}
}

// Masking never loses what the text's own view masks, wherever invisible or fullwidth characters
// land (mirrors --guarantee in the SDK-7219 reference): over every fixture input and its variants,
// the own view's spans reproduce the plain engine and sit inside the final merged spans.
func TestMaskingGuaranteeOverVariants(t *testing.T) {
	inserts := []string{"\u200b", "\u00ad", "\ufeff", "\U000e0020"}
	cases := loadRedactionFixture(t).Cases
	var inputs []string
	for _, c := range cases {
		if c.Repeat == 0 {
			inputs = append(inputs, c.Input)
		}
	}
	covered := func(spans []span) map[int]bool {
		m := map[int]bool{}
		for _, s := range spans {
			for p := s.s; p < s.e; p++ {
				m[p] = true
			}
		}
		return m
	}
	check := func(v string) string {
		own, ok := ownView(v).spans(redactRules, compiledRules)
		folded, ok2 := foldedView(v).spans(redactRules, compiledRules)
		if !ok || !ok2 {
			return "timed out"
		}
		if got, want := applySpans(v, own), maskPlain(redactRules, compiledRules, v); got != want {
			return fmt.Sprintf("own spans give %q, the plain engine %q", got, want)
		}
		merged := covered(mergeSpans(own, folded))
		for p := range covered(own) {
			if !merged[p] {
				return fmt.Sprintf("byte %d masked on the text itself is left unmasked", p)
			}
		}
		return ""
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	var count int
	work := make(chan string)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for in := range work {
				var variants []string
				variants = append(variants, in)
				cps := []rune(in)
				for i := 0; i <= len(cps); i++ {
					head, tail := string(cps[:i]), string(cps[i:])
					for _, ins := range inserts {
						variants = append(variants, head+ins+tail)
					}
					if i < len(cps) {
						switch c := cps[i]; {
						case c >= 0x21 && c <= 0x7e:
							variants = append(variants, head+string(c+0xFEE0)+string(cps[i+1:]))
						case c == ' ':
							variants = append(variants, head+"\u3000"+string(cps[i+1:]))
						}
					}
				}
				for _, v := range variants {
					msg := check(v)
					mu.Lock()
					count++
					if msg != "" && len(failures) < 10 {
						failures = append(failures, fmt.Sprintf("%q: %s", v, msg))
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, in := range inputs {
		work <- in
	}
	close(work)
	wg.Wait()
	for _, f := range failures {
		t.Error(f)
	}
	t.Logf("%d variants", count)
}

// The folded view of one code point is its own NFKC form, or nothing for a format character.
func TestFoldedViewIsNFKCPerCodePoint(t *testing.T) {
	for c := rune(0); c <= unicode.MaxRune; c++ {
		if c >= 0xD800 && c <= 0xDFFF {
			continue
		}
		want := norm.NFKC.String(string(c))
		if unicode.Is(formatCharacters, c) {
			want = ""
		}
		if got := string(foldedView(string(c)).chars); got != want {
			t.Fatalf("U+%04X folds to %q, want %q", c, got, want)
		}
	}
}
