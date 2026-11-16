package repository

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const fetchInstance = "0123456789abcdef0123456789abcdef"

func TestReadFetchManifest(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, input, format string
		branches            []bundleBranch
	}{
		{"sha1", "# v2 git bundle\n" + sha1 + " refs/heads/main\n\nPACK", "sha1", []bundleBranch{{"refs/heads/main", sha1}}},
		{"explicit sha1", "# v3 git bundle\n@object-format=sha1\n" + sha1 + " refs/heads/main\n\nPACK", "sha1", []bundleBranch{{"refs/heads/main", sha1}}},
		{"sha256", "# v3 git bundle\n@object-format=sha256\n" + sha256 + " refs/heads/feature/login\n\nPACK", "sha256", []bundleBranch{{"refs/heads/feature/login", sha256}}},
		{"shell and unicode", "# v2 git bundle\n" + sha1 + " refs/heads/topic/'$;()`é\n\n", "sha1", []bundleBranch{{"refs/heads/topic/'$;()`é", sha1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := readFetchManifest(strings.NewReader(tc.input))
			if err != nil || manifest.Format != tc.format || !reflect.DeepEqual(manifest.Branches, tc.branches) {
				t.Fatalf("manifest = %+v, %v; want %s %+v", manifest, err, tc.format, tc.branches)
			}
		})
	}
}

func TestFetchManifestBranchLimit(t *testing.T) {
	for _, count := range []int{4095, 4096, 4097} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			manifest, err := readFetchManifest(strings.NewReader(fetchManifestWithBranches(count)))
			if count > 4096 {
				if err == nil || len(manifest.Branches) != 0 {
					t.Fatalf("accepted %d branches: count=%d, error=%v", count, len(manifest.Branches), err)
				}
				return
			}
			if err != nil || len(manifest.Branches) != count {
				t.Fatalf("read %d branches: count=%d, error=%v", count, len(manifest.Branches), err)
			}
		})
	}
}

func fetchManifestWithBranches(count int) string {
	var manifest strings.Builder
	manifest.WriteString("# v2 git bundle\n")
	for i := range count {
		manifest.WriteString(strings.Repeat("a", 40))
		manifest.WriteString(" refs/heads/branch-")
		manifest.WriteString(strconv.Itoa(i))
		manifest.WriteString("\n")
	}
	manifest.WriteString("\n")
	return manifest.String()
}

func TestFetchManifestRejectsUntrustedMetadata(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct{ name, input string }{
		{"empty", ""},
		{"wrong version", "# v1 git bundle\n" + sha + " refs/heads/main\n\n"},
		{"empty branches", "# v2 git bundle\n\n"},
		{"truncated header", "# v2 git bundle\n" + sha + " refs/heads/main\n"},
		{"short oid", "# v2 git bundle\nabc refs/heads/main\n\n"},
		{"uppercase oid", "# v2 git bundle\n" + strings.Repeat("A", 40) + " refs/heads/main\n\n"},
		{"zero oid", "# v2 git bundle\n" + strings.Repeat("0", 40) + " refs/heads/main\n\n"},
		{"wrong format", "# v2 git bundle\n" + strings.Repeat("a", 64) + " refs/heads/main\n\n"},
		{"prerequisite", "# v2 git bundle\n-" + sha + " required base\n" + sha + " refs/heads/main\n\n"},
		{"filtered", "# v3 git bundle\n@filter=blob:none\n" + sha + " refs/heads/main\n\n"},
		{"duplicate capability", "# v3 git bundle\n@object-format=sha1\n@object-format=sha1\n" + sha + " refs/heads/main\n\n"},
		{"late capability", "# v3 git bundle\n" + sha + " refs/heads/main\n@object-format=sha1\n\n"},
		{"tag", "# v2 git bundle\n" + sha + " refs/tags/secret\n\n"},
		{"head", "# v2 git bundle\n" + sha + " HEAD\n\n"},
		{"duplicate", "# v2 git bundle\n" + sha + " refs/heads/main\n" + sha + " refs/heads/main\n\n"},
		{"long line", "# v2 git bundle\n" + strings.Repeat("x", 65537)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := readFetchManifest(strings.NewReader(tc.input))
			if err == nil || len(manifest.Branches) != 0 {
				t.Fatalf("accepted metadata: %+v %v", manifest, err)
			}
		})
	}
}

func TestFetchRefNamespace(t *testing.T) {
	for _, branch := range []string{"main", "feature/login", "topic/'$;()`é", "-not-an-option"} {
		t.Run(branch, func(t *testing.T) {
			ref, err := fetchDestinationRef(fetchInstance, "refs/heads/"+branch)
			if err != nil || ref != "refs/remotes/hk/"+fetchInstance+"/"+branch {
				t.Fatalf("mapped ref = %q, %v", ref, err)
			}
		})
	}
	for _, ref := range []string{"", "--help", "main", "HEAD", "refs/heads/", "refs/tags/main", "refs/heads/../main", "refs/heads/.hidden", "refs/heads/a.lock", "refs/heads/a//b", "refs/heads/a\\b", "refs/heads/a\nb", "refs/heads/a\x00b", "refs/heads/a\x7fb", "refs/heads/a\u0085b", "refs/heads/a b", "refs/heads/a@{b", "refs/heads/a.", "refs/heads/a:b", "refs/heads/a?b", "refs/heads/a*b", "refs/heads/a[b", "refs/heads/a^b", "refs/heads/a~b"} {
		t.Run(ref, func(t *testing.T) {
			if got, err := fetchDestinationRef(fetchInstance, ref); err == nil || got != "" {
				t.Fatalf("accepted unsafe ref %q as %q: %v", ref, got, err)
			}
		})
	}
	for _, id := range []string{"", "../escape", "--help", strings.Repeat("0", 31), strings.Repeat("A", 32), strings.Repeat("g", 32), fetchInstance + "\n"} {
		if got, err := fetchDestinationRef(id, "refs/heads/main"); err == nil || got != "" {
			t.Fatalf("accepted instance %q as %q: %v", id, got, err)
		}
	}
}

func TestFetchRefLengthLimit(t *testing.T) {
	for _, size := range []int{4095, 4096, 4097} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ref := "refs/heads/" + strings.Repeat("a", size-len("refs/heads/"))
			destination, err := fetchDestinationRef(fetchInstance, ref)
			if size > 4096 {
				if err == nil || destination != "" {
					t.Fatalf("accepted ref with %d bytes: %v", size, err)
				}
				return
			}
			want := "refs/remotes/hk/" + fetchInstance + "/" + strings.TrimPrefix(ref, "refs/heads/")
			if err != nil || destination != want {
				t.Fatalf("mapping ref with %d bytes: matches=%v, error=%v", size, destination == want, err)
			}
		})
	}
}

func FuzzFetchManifest(f *testing.F) {
	for _, seed := range []string{"", "# v2 git bundle\n" + strings.Repeat("a", 40) + " refs/heads/main\n\n", "# v3 git bundle\n@object-format=sha256\n" + strings.Repeat("b", 64) + " refs/heads/feature/login\n\n", "# v2 git bundle\n-000 refs/heads/../escape\n\n"} {
		f.Add(seed)
	}
	f.Add("# v3 git bundle\n@object-format=sha1\n" + strings.Repeat("a", 40) + " refs/heads/main\n\n")
	for _, count := range []int{4095, 4096, 4097} {
		f.Add(fetchManifestWithBranches(count))
	}
	f.Fuzz(func(t *testing.T, input string) {
		manifest, err := readFetchManifest(strings.NewReader(input))
		if err != nil {
			if len(manifest.Branches) != 0 {
				t.Fatal("failed parse exposed branch metadata")
			}
			return
		}
		if manifest.Format != "sha1" && manifest.Format != "sha256" || len(manifest.Branches) == 0 || len(manifest.Branches) > 4096 {
			t.Fatalf("invalid accepted manifest: %+v", manifest)
		}
		seen := map[string]bool{}
		for _, branch := range manifest.Branches {
			ref, err := fetchDestinationRef(fetchInstance, branch.Ref)
			if err != nil || !strings.HasPrefix(ref, "refs/remotes/hk/"+fetchInstance+"/") || seen[ref] {
				t.Fatalf("accepted unsafe or duplicate branch: %+v", branch)
			}
			seen[ref] = true
		}
	})
}

func FuzzFetchNamespace(f *testing.F) {
	for _, ref := range []string{"refs/heads/main", "refs/heads/feature/é", "--help", "refs/heads/../../heads/main", "refs/heads/a\x00b", "refs/heads/a@{b", "refs/tags/private"} {
		f.Add(fetchInstance, ref)
	}
	for _, size := range []int{4095, 4096, 4097} {
		f.Add(fetchInstance, "refs/heads/"+strings.Repeat("a", size-len("refs/heads/")))
	}
	f.Fuzz(func(t *testing.T, instance, source string) {
		ref, err := fetchDestinationRef(instance, source)
		if err != nil {
			if ref != "" {
				t.Fatal("failed mapping exposed destination")
			}
			return
		}
		if len(instance) != 32 || len(source) > 4096 || !strings.HasPrefix(ref, "refs/remotes/hk/"+instance+"/") || strings.ContainsAny(ref, "\x00\r\n\\") || strings.Contains(ref, "..") {
			t.Fatalf("namespace escaped: %q %q => %q", instance, source, ref)
		}
	})
}
