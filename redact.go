package rudderstackai

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

// Built-in masking of opt-in text (call_reason, capability_detail). The rules are shared with
// the Node.js and Python SDKs through spec/fixtures/redaction-v0.1.json; regexp2 is used because
// the patterns rely on lookbehind, which RE2 lacks.

// maxMaskedCodePoints: longer text is withheld rather than masked.
const maxMaskedCodePoints = 32_000

// redactMatchTimeout bounds each pattern's run over one text; regexp2 backtracks, so a crafted
// text could otherwise hold the caller for seconds.
const redactMatchTimeout = 100 * time.Millisecond

// redactionTimedOut replaces a field whose masking ran out of time: never sent unmasked.
// Identical in Node.js and Python.
const redactionTimedOut = "[redaction timed out]"

type redactRule struct {
	name    string
	pattern string
	token   string
	check   func(string) bool
	keep    bool
}

var redactRules = []redactRule{
	{name: "pem_private_key", pattern: `-----BEGIN[A-Z0-9 ]*PRIVATE KEY-----(?:[^-]|-(?!----))*(?:-----END[A-Z0-9 ]*PRIVATE KEY-----)?`, token: "secret"},
	{name: "bearer", pattern: `(?<![A-Za-z0-9_-])[Bb]earer[ \t]+[A-Za-z0-9._~+/-]{16,}=*`, token: "secret"},
	{name: "jwt", pattern: `(?<![A-Za-z0-9_-])eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`, token: "secret"},
	{name: "openai_anthropic_key", pattern: `(?<![A-Za-z0-9_-])sk-[A-Za-z0-9_-]{16,}`, token: "secret"},
	{name: "stripe_key", pattern: `(?<![A-Za-z0-9_-])(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{10,}`, token: "secret"},
	{name: "google_api_key", pattern: `(?<![A-Za-z0-9_-])AIza[0-9A-Za-z_-]{35}(?![0-9A-Za-z_-])`, token: "secret"},
	{name: "google_oauth_token", pattern: `(?<![A-Za-z0-9_-])ya29\.[0-9A-Za-z_-]{20,}`, token: "secret"},
	{name: "slack_token", pattern: `(?<![A-Za-z0-9_-])xox[abposr]-[A-Za-z0-9-]{10,}`, token: "secret"},
	{name: "github_token", pattern: `(?<![A-Za-z0-9_-])gh[pousr]_[A-Za-z0-9]{20,}`, token: "secret"},
	{name: "aws_access_key", pattern: `(?<![A-Za-z0-9])(?:AKIA|ASIA)[A-Z0-9]{16}(?![A-Za-z0-9])`, token: "secret"},
	{name: "url_password", pattern: `((?<![A-Za-z0-9+.-])[A-Za-z][A-Za-z0-9+.-]*://[^ \t\r\n:/@]*:)[^ \t\r\n/@]+(?=@)`, token: "secret", keep: true},
	{name: "key_value_secret", pattern: `((?<![A-Za-z0-9_-])(?:[Aa][Pp][Ii][_-]?[Kk][Ee][Yy]|[Aa][Cc][Cc][Ee][Ss][Ss][_-]?[Tt][Oo][Kk][Ee][Nn]|[Aa][Uu][Tt][Hh][_-]?[Tt][Oo][Kk][Ee][Nn]|[Cc][Ll][Ii][Ee][Nn][Tt][_-]?[Ss][Ee][Cc][Rr][Ee][Tt]|[Ss][Ee][Cc][Rr][Ee][Tt][_-]?[Kk][Ee][Yy]|[Tt][Oo][Kk][Ee][Nn]|[Pp][Aa][Ss][Ss][Ww][Oo][Rr][Dd]|[Pp][Aa][Ss][Ss][Ww][Dd]|[Pp][Ww][Dd])["']?[ \t]*[:=][ \t]*["']?)[^ \t\r\n"'&,;]{4,}`, token: "secret", keep: true},
	{name: "email", pattern: `(?<![A-Za-z0-9._%+\u00C0-\uD7FF-])[A-Za-z0-9._%+\u00C0-\uD7FF-]+@[A-Za-z0-9\u00C0-\uD7FF-]+(?:\.[A-Za-z0-9\u00C0-\uD7FF-]+)*\.[A-Za-z\u00C0-\uD7FF]{2,}`, token: "email"},
	{name: "ipv4", pattern: `(?<![A-Za-z0-9.])(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])(?![0-9]|\.[0-9])`, token: "ip"},
	{name: "ipv6", pattern: `(?<![A-Za-z0-9:.])(?:[0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4}(?![A-Za-z0-9:])`, token: "ip", check: checkIPv6},
	{name: "iban", pattern: `(?<![A-Za-z0-9])[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?(?![A-Za-z0-9])`, token: "iban", check: checkIBAN},
	{name: "us_ssn", pattern: `(?<![0-9-])[0-9]{3}-[0-9]{2}-[0-9]{4}(?![0-9-])`, token: "ssn", check: checkSSN},
	{name: "card", pattern: `(?<![0-9])[0-9](?:[ -]?[0-9]){12,18}(?![0-9])`, token: "card", check: checkLuhn},
	{name: "phone", pattern: `(?<![A-Za-z0-9+])(?<![0-9][ .-])(?:\+[0-9]{1,3}(?:[ .-]?(?:\([0-9]{1,4}\)|[0-9]{1,4})){2,6}|(?:\([0-9]{2,4}\)[ .-]?|[0-9]{2,4}[ .-])[0-9]{3,4}[ .-][0-9]{3,4}|[0-9]{4,5}[ .-][0-9]{5,6})(?![0-9A-Za-z]|[ .-][0-9])`, token: "phone", check: checkPhone},
}

var compiledRules = func() []*regexp2.Regexp {
	out := make([]*regexp2.Regexp, len(redactRules))
	for i, r := range redactRules {
		out[i] = regexp2.MustCompile(r.pattern, regexp2.None)
		out[i].MatchTimeout = redactMatchTimeout
	}
	return out
}()

func digits(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func checkLuhn(m string) bool {
	ds := digits(m)
	sum := 0
	for i := 0; i < len(ds); i++ {
		d := int(ds[len(ds)-1-i] - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// Exactly 8 groups, or fewer with a single "::".
func checkIPv6(m string) bool {
	if !strings.ContainsAny(m, "0123456789abcdefABCDEF") {
		return false
	}
	halves := strings.Split(m, "::")
	if len(halves) > 2 {
		return false
	}
	var parts []string
	for _, h := range halves {
		if h != "" {
			parts = append(parts, strings.Split(h, ":")...)
		}
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	if len(halves) == 2 {
		return len(parts) <= 7
	}
	return len(parts) == 8
}

func checkPhone(m string) bool {
	n := len(digits(m))
	if strings.HasPrefix(m, "+") {
		return n >= 9 && n <= 15
	}
	return n >= 10 && n <= 12
}

// Area 000, 666 and 900-999, group 00 and serial 0000 are never issued.
func checkSSN(m string) bool {
	p := strings.Split(m, "-")
	return len(p) == 3 && p[0] != "000" && p[0] != "666" && !strings.HasPrefix(p[0], "9") && p[1] != "00" && p[2] != "0000"
}

// ISO 13616: country and check digits moved to the end, letters as 10..35, mod 97 is 1.
func checkIBAN(m string) bool {
	compact := strings.ReplaceAll(m, " ", "")
	if len(compact) < 15 || len(compact) > 34 {
		return false
	}
	rest := 0
	for _, c := range compact[4:] + compact[:4] {
		if c >= 'A' && c <= 'Z' {
			v := int(c - 55)
			rest = (rest*100 + v) % 97
		} else {
			rest = (rest*10 + int(c-'0')) % 97
		}
	}
	return rest == 1
}

// maskBuiltIn replaces emails, phone numbers, cards, IPs, IDs and keys/tokens with tokens. When a
// pattern runs out of time the whole text becomes redactionTimedOut (reported).
func maskBuiltIn(text string) string { return maskWith(redactRules, compiledRules, text) }

func maskWith(rules []redactRule, compiled []*regexp2.Regexp, text string) string {
	out := text
	for i, r := range rules {
		token := "[" + r.token + "]"
		replaced, err := compiled[i].ReplaceFunc(out, func(m regexp2.Match) string {
			s := m.String()
			if r.check != nil && !r.check(s) {
				return s
			}
			if r.keep {
				return m.GroupByNumber(1).String() + token
			}
			return token
		}, -1, -1)
		if err != nil {
			report(KindRedactionTimeout, "redaction pattern "+r.name, err)
			return redactionTimedOut
		}
		out = replaced
	}
	return out
}

func hasSensitive(text string) bool { return maskBuiltIn(text) != text }

// maskText masks one opt-in text field; ok is false when the text is too long to mask and
// must be withheld.
func maskText(text string) (string, bool) {
	if utf8.RuneCountInString(text) > maxMaskedCodePoints {
		return "", false
	}
	return maskBuiltIn(text), true
}
