package rudderstackai

import (
	"strings"
	"testing"
	"unicode/utf8"
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
