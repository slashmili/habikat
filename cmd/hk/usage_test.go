package main

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrintableText(t *testing.T) {
	for input, want := range map[string]string{
		`invalid argument "maybe" for "--force"`: `invalid argument "maybe" for "--force"`,
		"--bad\n\x1b[2J":                         `--bad\n\x1b[2J`,
		"rtl\u202eoverride":                      `rtl\u202eoverride`,
		"bad byte \xff":                          `bad byte \xff`,
		`back\slash`:                             `back\slash`,
	} {
		if got := printableText(input); got != want {
			t.Fatalf("printableText(%q) = %q, want %q", input, got, want)
		}
	}
}

func FuzzPrintableText(f *testing.F) {
	for _, seed := range []string{"", "plain", "a\nb", "\x1b[2J", "‮", "\xff\xfe", `"quoted"`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		printable := printableText(text)
		assertNoControl(t, printable, false)
		if !utf8.ValidString(printable) {
			t.Fatalf("invalid UTF-8 in %q", printable)
		}
		alreadyPrintable := utf8.ValidString(text) && !strings.ContainsFunc(text, isUnprintable)
		if alreadyPrintable != (printable == text) {
			t.Fatalf("printableText(%q) = %q", text, printable)
		}
	})
}

func isUnprintable(r rune) bool {
	return !strconv.IsGraphic(r)
}
