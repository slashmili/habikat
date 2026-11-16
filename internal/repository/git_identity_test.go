package repository

import (
	"strings"
	"testing"
)

func TestGitIdentityValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity GitIdentity
		valid    bool
	}{
		{"ordinary", GitIdentity{"Test User", "test+fixture@example.invalid"}, true},
		{"unicode and shell", GitIdentity{"Zoë 李 O'Neil $HOME `id`;", "quote'$+tag@example.invalid"}, true},
		{"missing name", GitIdentity{"", "test@example.invalid"}, false},
		{"blank email", GitIdentity{"User", " \t "}, false},
		{"newline", GitIdentity{"User\nextra", "test@example.invalid"}, false},
		{"carriage return", GitIdentity{"User", "test\r@example.invalid"}, false},
		{"nul", GitIdentity{"User\x00", "test@example.invalid"}, false},
		{"angle brackets", GitIdentity{"User <admin>", "test@example.invalid"}, false},
		{"Git rejects punctuation-only name", GitIdentity{",:;\"\\'", "test@example.invalid"}, false},
		{"email becomes empty", GitIdentity{"User", ",:;\"\\'"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.identity.Validate(); (err == nil) != tc.valid {
				t.Fatalf("validation error=%v, valid=%v", err, tc.valid)
			}
		})
	}
}

func FuzzGitIdentityValidation(f *testing.F) {
	for _, value := range []string{"User", "Zoë 李 $`'", "", " \t", "bad\x00name", "bad\rname", "bad\nname", "a<b>"} {
		f.Add(value, "test+tag@example.invalid")
	}
	f.Fuzz(func(t *testing.T, name, email string) {
		identity := GitIdentity{Name: name, Email: email}
		if identity.Validate() != nil {
			return
		}
		for _, value := range []string{name, email} {
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n<>") {
				t.Fatalf("unsafe identity accepted: %q", value)
			}
		}
	})
}
