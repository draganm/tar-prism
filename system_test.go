package tarprysm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type systemTar struct {
	path string
	kind string // "gnu" or "bsd"
}

// findSystemTars locates one GNU tar and one bsdtar, whichever are present.
// Inside the nix dev shell "tar" is GNU tar; macOS ships bsdtar at
// /usr/bin/tar.
func findSystemTars() []systemTar {
	var found []systemTar
	seen := map[string]bool{}
	for _, candidate := range []string{"tar", "gtar", "bsdtar", "/usr/bin/tar"} {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		out, err := exec.Command(path, "--version").CombinedOutput()
		if err != nil {
			continue
		}
		kind := ""
		switch {
		case bytes.Contains(out, []byte("GNU tar")):
			kind = "gnu"
		case bytes.Contains(out, []byte("bsdtar")):
			kind = "bsd"
		}
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		found = append(found, systemTar{path: path, kind: kind})
	}
	return found
}

// makeTree creates a directory tree with short and long names, a symlink, a
// hard link, an empty file, a sparse file, and nested directories. When long
// is set it adds a path too long for the ustar format.
func makeTree(t *testing.T, long bool) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "src")
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string, data []byte) {
		if err := os.WriteFile(filepath.Join(root, rel), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prefixDir := strings.Repeat("p", 96)
	mkdir("dir/nested")
	mkdir(prefixDir)
	write("short.txt", []byte("hello\n"))
	write("empty.txt", nil)
	write("dir/"+strings.Repeat("n", 90)+".txt", []byte("name fits in 100 bytes\n"))
	write(prefixDir+"/f.txt", []byte("needs the ustar prefix field\n"))
	write("dir/nested/deep.bin", randomBytes(100<<10))
	if err := os.Symlink("short.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "short.txt"), filepath.Join(root, "hard")); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.Create(filepath.Join(root, "sparse.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(4 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := sparse.WriteAt([]byte("data in the middle"), 2<<20); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}
	if long {
		deep := strings.Repeat("d/", 140)
		mkdir(deep)
		write(deep+"leaf.txt", []byte("deep\n"))
	}
	return root
}

func TestRoundTripSystemTar(t *testing.T) {
	tars := findSystemTars()
	if len(tars) == 0 {
		t.Skip("no tar binary found")
	}
	type variant struct {
		name string
		args []string
		long bool
	}
	variants := map[string][]variant{
		"gnu": {
			{"ustar", []string{"--format=ustar"}, false},
			{"posix", []string{"--format=posix"}, true},
			{"gnu", []string{"--format=gnu"}, true},
			{"gnu-sparse", []string{"--format=gnu", "-S"}, true},
			{"posix-sparse", []string{"--format=posix", "-S"}, true},
		},
		"bsd": {
			{"ustar", []string{"--format", "ustar"}, false},
			{"pax", []string{"--format", "pax"}, true},
			{"gnutar", []string{"--format", "gnutar"}, true},
		},
	}
	for _, st := range tars {
		for _, v := range variants[st.kind] {
			t.Run(st.kind+"/"+v.name, func(t *testing.T) {
				src := makeTree(t, v.long)
				out := filepath.Join(t.TempDir(), "archive.tar")
				args := append(append([]string{}, v.args...), "-cf", out, "-C", src, ".")
				if output, err := exec.Command(st.path, args...).CombinedOutput(); err != nil {
					t.Fatalf("%s %v: %v\n%s", st.path, args, err, output)
				}
				archive, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				composed, _, idx := roundTrip(t, archive)
				assertIdentical(t, archive, composed)
				if len(idx.Entries) < 6 {
					t.Errorf("only %d blobs extracted from a %d-byte archive: %+v", len(idx.Entries), len(archive), idx.Entries)
				}
			})
		}
	}
}
