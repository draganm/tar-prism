package tarprysm

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeErrors(t *testing.T) {
	posix := "ustar\x0000"
	archive := concat(
		rawHeader{name: "a", typeflag: '0', size: 3, magic: posix}.block(), payload([]byte("abc")),
		rawHeader{name: "b", typeflag: '0', size: 3, magic: posix}.block(), payload([]byte("def")),
		endMarker)

	fresh := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "prysm")
		if err := Decompose(bytes.NewReader(archive), dir); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	editIndex := func(t *testing.T, dir string, edit func(*Index)) {
		t.Helper()
		idx, err := ReadIndex(dir)
		if err != nil {
			t.Fatal(err)
		}
		edit(idx)
		if err := writeIndex(dir, idx); err != nil {
			t.Fatal(err)
		}
	}
	blob := func(dir string, n int) string {
		return filepath.Join(dir, BlobsDir, fmt.Sprintf("%08d", n))
	}
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		tamper  func(t *testing.T, dir string)
		wantErr string
	}{
		{"missing blob", func(t *testing.T, dir string) { must(t, os.Remove(blob(dir, 2))) }, "no such file"},
		{"blob size mismatch", func(t *testing.T, dir string) { must(t, os.WriteFile(blob(dir, 1), []byte("abcd"), 0o644)) }, "is 4 bytes, index says 3"},
		{"tampered blob", func(t *testing.T, dir string) { must(t, os.WriteFile(blob(dir, 1), []byte("xyz"), 0o644)) }, "digest"},
		{"tampered recipe", func(t *testing.T, dir string) {
			f, err := os.OpenFile(filepath.Join(dir, RecipeFile), os.O_WRONLY, 0)
			must(t, err)
			_, err = f.WriteAt([]byte("z"), 0)
			must(t, err)
			must(t, f.Close())
		}, "digest"},
		{"unsupported version", func(t *testing.T, dir string) { editIndex(t, dir, func(i *Index) { i.Version = 2 }) }, "unsupported version"},
		{"offsets out of order", func(t *testing.T, dir string) {
			editIndex(t, dir, func(i *Index) { i.Entries[0], i.Entries[1] = i.Entries[1], i.Entries[0] })
		}, "does not increase"},
		{"truncated recipe", func(t *testing.T, dir string) { must(t, os.Truncate(filepath.Join(dir, RecipeFile), 700)) }, "recipe ends"},
		{"missing index", func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, IndexFile))) }, "reading index"},
		{"missing recipe", func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, RecipeFile))) }, "opening recipe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := fresh(t)
			tc.tamper(t, dir)
			err := Compose(dir, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestComposeToFile(t *testing.T) {
	archive := concat(rawHeader{name: "a", typeflag: '0', size: 3, magic: "ustar\x0000"}.block(), payload([]byte("abc")), endMarker)
	dir := filepath.Join(t.TempDir(), "prysm")
	if err := Decompose(bytes.NewReader(archive), dir); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.tar")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := Compose(dir, f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	assertIdentical(t, archive, got)
}
