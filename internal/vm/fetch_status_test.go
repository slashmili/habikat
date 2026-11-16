package vm

import "testing"

func TestParseFetchStatus(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		want         GuestChanges
	}{
		{name: "clean"},
		{name: "staged", status: "A  added\x00M  edited\x00D  removed\x00", want: GuestChanges{Staged: true}},
		{name: "unstaged", status: " M file\x00 D removed\x00 T mode\x00", want: GuestChanges{Unstaged: true}},
		{name: "untracked", status: "?? new\x00", want: GuestChanges{Untracked: true}},
		{name: "all", status: "MM file\x00?? new\x00", want: GuestChanges{Staged: true, Unstaged: true, Untracked: true}},
		{name: "rename", status: "R  new name\x00old name\x00", want: GuestChanges{Staged: true}},
		{name: "conflict", status: "UU conflicted\x00", want: GuestChanges{Staged: true, Unstaged: true}},
		{name: "path bytes", status: "?? path\n\r\xff\t$;()\x00", want: GuestChanges{Untracked: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFetchStatus([]byte(tc.status))
			if err != nil || got != tc.want {
				t.Fatalf("status = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestParseFetchStatusRejectsAmbiguity(t *testing.T) {
	for _, status := range []string{"clean\n", " M file", "\x00", "?? \x00", "!! file\x00", " M\x00", " Mxfile\x00", "   file\x00", "?M file\x00", "R  file\x00", "R  file\x00\x00", "M? file\x00"} {
		t.Run(status, func(t *testing.T) {
			if _, err := parseFetchStatus([]byte(status)); err == nil {
				t.Fatalf("accepted malformed status %q", status)
			}
		})
	}
}

func FuzzFetchStatus(f *testing.F) {
	for _, seed := range []string{"", "MM file\x00?? other\x00", "R  new\x00old\x00", "?? \x00", " M incomplete", "!! ignored\x00", "?? newline\nname\x00"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxGuestOutput {
			t.Skip()
		}
		got, err := parseFetchStatus(data)
		if err != nil {
			return
		}
		if len(data) > 0 && (!got.Dirty() || data[len(data)-1] != 0) {
			t.Fatal("nonempty accepted status cannot be clean or truncated")
		}
		if len(data) == 0 && got.Dirty() {
			t.Fatal("empty status reported changes")
		}
		if _, err := parseFetchStatus(append(append([]byte{}, data...), 0)); err == nil {
			t.Fatal("trailing empty record accepted")
		}
	})
}
