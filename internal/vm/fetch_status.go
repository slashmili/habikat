package vm

import (
	"bytes"
	"errors"
	"strings"
)

type GuestChanges struct {
	Staged, Unstaged, Untracked bool
}

func (changes GuestChanges) Dirty() bool {
	return changes.Staged || changes.Unstaged || changes.Untracked
}

func parseFetchStatus(data []byte) (GuestChanges, error) {
	var changes GuestChanges
	for len(data) > 0 {
		entry, rest, found := bytes.Cut(data, []byte{0})
		if !found || len(entry) < 4 || entry[2] != ' ' {
			return GuestChanges{}, errors.New("invalid guest Git status")
		}
		data = rest
		if string(entry[:2]) == "??" {
			changes.Untracked = true
			continue
		}
		if !validStatusFlags(entry[0], entry[1]) {
			return GuestChanges{}, errors.New("invalid guest Git status flags")
		}
		changes.Staged = changes.Staged || entry[0] != ' '
		changes.Unstaged = changes.Unstaged || entry[1] != ' '
		if bytes.ContainsAny(entry[:2], "RC") {
			original, rest, found := bytes.Cut(data, []byte{0})
			if !found || len(original) == 0 {
				return GuestChanges{}, errors.New("incomplete guest rename status")
			}
			data = rest
		}
	}
	return changes, nil
}

func validStatusFlags(index, worktree byte) bool {
	return (index != ' ' || worktree != ' ') && strings.ContainsRune(" MADRCUT", rune(index)) && strings.ContainsRune(" MADRCUT", rune(worktree))
}
