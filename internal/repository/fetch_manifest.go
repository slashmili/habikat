package repository

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

type bundleBranch struct {
	Ref    string
	Commit string
}

type bundleManifest struct {
	Format   string
	Branches []bundleBranch
}

const maxFetchManifest = 1 << 20
const maxFetchBranches = 4096

func readFetchManifest(input io.Reader) (bundleManifest, error) {
	reader := bufio.NewReaderSize(io.LimitReader(input, maxFetchManifest), 64<<10)
	line, err := readBundleLine(reader)
	if err != nil || line != "# v2 git bundle" && line != "# v3 git bundle" {
		return bundleManifest{}, errors.New("invalid Git bundle header")
	}
	return readBundleBranches(reader, line == "# v3 git bundle")
}

func readBundleLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return "", fmt.Errorf("incomplete or oversized bundle manifest: %w", err)
	}
	return string(line[:len(line)-1]), nil
}

func readBundleBranches(reader *bufio.Reader, version3 bool) (bundleManifest, error) {
	manifest := bundleManifest{Format: "sha1"}
	seen := map[string]bool{}
	formatSet := false
	for {
		line, err := readBundleLine(reader)
		if err != nil {
			return bundleManifest{}, err
		}
		if line == "" {
			if len(manifest.Branches) == 0 {
				return bundleManifest{}, errors.New("bundle has no local branches")
			}
			return manifest, nil
		}
		if strings.HasPrefix(line, "@") {
			if !version3 || formatSet || len(manifest.Branches) != 0 || line != "@object-format=sha1" && line != "@object-format=sha256" {
				return bundleManifest{}, errors.New("unsupported bundle capability")
			}
			manifest.Format = strings.TrimPrefix(line, "@object-format=")
			formatSet = true
			continue
		}
		branch, err := parseBundleBranch(line, manifest.Format)
		if err != nil {
			return bundleManifest{}, err
		}
		if seen[branch.Ref] || len(manifest.Branches) >= maxFetchBranches {
			return bundleManifest{}, errors.New("duplicate branch or too many branches in bundle manifest")
		}
		seen[branch.Ref] = true
		manifest.Branches = append(manifest.Branches, branch)
	}
}

func parseBundleBranch(line, format string) (bundleBranch, error) {
	if strings.HasPrefix(line, "-") {
		return bundleBranch{}, errors.New("bundle must be self-contained without prerequisites")
	}
	commit, ref, found := strings.Cut(line, " ")
	if !found || !validObjectID(commit, format) || validateFetchBranch(ref) != nil {
		return bundleBranch{}, errors.New("invalid branch or commit in bundle manifest")
	}
	return bundleBranch{Ref: ref, Commit: commit}, nil
}

func validObjectID(value, format string) bool {
	size := 40
	if format == "sha256" {
		size = 64
	} else if format != "sha1" {
		return false
	}
	return validHex(value, size) && strings.Trim(value, "0") != ""
}

func validHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func fetchDestinationRef(instanceID, ref string) (string, error) {
	if !validHex(instanceID, 32) {
		return "", errors.New("invalid VM instance identifier")
	}
	if err := validateFetchBranch(ref); err != nil {
		return "", err
	}
	return "refs/remotes/hk/" + instanceID + "/" + strings.TrimPrefix(ref, "refs/heads/"), nil
}

func validateFetchBranch(ref string) error {
	if !strings.HasPrefix(ref, "refs/heads/") || len(ref) > 4096 || !utf8.ValidString(ref) || strings.ContainsAny(ref, " ~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, ".") {
		return errors.New("invalid local branch ref")
	}
	for _, c := range ref {
		if unicode.IsControl(c) {
			return errors.New("control character in local branch ref")
		}
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return errors.New("invalid local branch ref component")
		}
	}
	return nil
}
