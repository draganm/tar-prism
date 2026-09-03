package tarprism

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// roundTrip decomposes archive into a fresh prism directory and composes it
// back, returning the composed bytes, the prism directory, and its index. It
// also runs the archive through a memory sink and source and checks that
// both paths agree on the output and the index.
func roundTrip(t *testing.T, archive []byte) ([]byte, string, *Index) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "prism")
	if err := Decompose(bytes.NewReader(archive), dir); err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	idx, err := ReadIndex(dir)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	var out bytes.Buffer
	if err := Compose(dir, &out); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	memOut, mem := memoryRoundTrip(t, archive)
	assertIdentical(t, out.Bytes(), memOut)
	if !reflect.DeepEqual(mem.index, idx) {
		t.Fatalf("memory index %+v differs from directory index %+v", mem.index, idx)
	}
	return out.Bytes(), dir, idx
}

// assertIdentical fails unless got is byte-for-byte equal to want.
func assertIdentical(t *testing.T, want, got []byte) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	t.Fatalf("composed archive differs from original: length %d vs %d, first difference at byte %d", len(want), len(got), i)
}

// assertBlobs checks that the index lists exactly the given names, in order,
// and that each blob file in dir holds the corresponding content.
func assertBlobs(t *testing.T, dir string, idx *Index, names []string, contents [][]byte) {
	t.Helper()
	assertEntries(t, idx, names, contents, func(e Entry) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Blob)))
	})
}

// assertEntries checks that the index lists exactly the given names, in
// order, and that each blob, fetched with read, holds the corresponding
// content.
func assertEntries(t *testing.T, idx *Index, names []string, contents [][]byte, read func(Entry) ([]byte, error)) {
	t.Helper()
	if len(idx.Entries) != len(names) {
		t.Fatalf("index has %d entries, want %d: %+v", len(idx.Entries), len(names), idx.Entries)
	}
	for i, e := range idx.Entries {
		if e.Name != names[i] {
			t.Errorf("entry %d: name %q, want %q", i, e.Name, names[i])
		}
		if e.Blob != blobName(i+1) {
			t.Errorf("entry %d: blob %q, want %q", i, e.Blob, blobName(i+1))
		}
		got, err := read(e)
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if !bytes.Equal(got, contents[i]) {
			t.Errorf("entry %d (%s): blob content differs (%d bytes, want %d)", i, e.Name, len(got), len(contents[i]))
		}
		if e.Size != int64(len(contents[i])) {
			t.Errorf("entry %d (%s): size %d, want %d", i, e.Name, e.Size, len(contents[i]))
		}
	}
}

type testFile struct {
	hdr  tar.Header
	body []byte
}

// buildTar writes files with archive/tar in the given format.
func buildTar(t *testing.T, format tar.Format, files []testFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		h := f.hdr
		h.Format = format
		h.Size = int64(len(f.body))
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		h.ModTime = time.Unix(1700000000, 0)
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("WriteHeader(%s): %v", h.Name, err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatalf("Write(%s): %v", h.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// randomBytes returns n deterministic pseudo-random bytes.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	x := uint32(42)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// generatedFiles returns the file set that needs PAX records or GNU
// long-name entries: everything in commonFiles plus a deep path, a long link
// target, and a file after them.
func generatedFiles(t *testing.T) []testFile {
	t.Helper()
	deepName := strings.Repeat("d/", 140) + "leaf.txt" // > 255 bytes: needs PAX path or GNU 'L'
	longLink := strings.Repeat("t", 150)               // > 100 bytes: needs PAX linkpath or GNU 'K'
	return append(commonFiles(),
		testFile{hdr: tar.Header{Name: deepName, Typeflag: tar.TypeReg}, body: []byte("deep\n")},
		testFile{hdr: tar.Header{Name: "longlink", Typeflag: tar.TypeSymlink, Linkname: longLink}},
		testFile{hdr: tar.Header{Name: "after.txt", Typeflag: tar.TypeReg}, body: []byte("after\n")},
	)
}

// commonFiles returns a file set that fits every format, including ustar's
// prefix/name split.
func commonFiles() []testFile {
	big := randomBytes(3<<20 + 17)
	prefixName := strings.Repeat("d", 120) + "/file.txt" // needs the ustar prefix field
	return []testFile{
		{hdr: tar.Header{Name: "a.txt", Typeflag: tar.TypeReg}, body: []byte("hello\n")},
		{hdr: tar.Header{Name: "empty.txt", Typeflag: tar.TypeReg}},
		{hdr: tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "exact.bin", Typeflag: tar.TypeReg}, body: randomBytes(1024)},
		{hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "a.txt"}},
		{hdr: tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "a.txt"}},
		{hdr: tar.Header{Name: prefixName, Typeflag: tar.TypeReg}, body: []byte("prefixed\n")},
		{hdr: tar.Header{Name: "big.bin", Typeflag: tar.TypeReg}, body: big},
		{hdr: tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}},
	}
}

// generatedArchive is an archive written by archive/tar with the regular
// files it must yield.
type generatedArchive struct {
	name     string
	archive  []byte
	names    []string
	contents [][]byte
}

// generatedArchives returns archive/tar output in ustar, PAX and GNU format,
// each also padded to a 10240-byte record boundary.
func generatedArchives(t *testing.T) []generatedArchive {
	t.Helper()
	regularFiles := func(files []testFile) (names []string, bodies [][]byte) {
		for _, f := range files {
			if f.hdr.Typeflag == tar.TypeReg {
				names = append(names, f.hdr.Name)
				bodies = append(bodies, f.body)
			}
		}
		return names, bodies
	}
	cases := []struct {
		name   string
		format tar.Format
		files  []testFile
	}{
		{"ustar", tar.FormatUSTAR, commonFiles()},
		{"pax", tar.FormatPAX, generatedFiles(t)},
		{"gnu", tar.FormatGNU, generatedFiles(t)},
	}
	var out []generatedArchive
	for _, tc := range cases {
		archive := buildTar(t, tc.format, tc.files)
		names, bodies := regularFiles(tc.files)
		padded := append(append([]byte{}, archive...), make([]byte, 10240-len(archive)%10240)...)
		out = append(out,
			generatedArchive{name: tc.name, archive: archive, names: names, contents: bodies},
			generatedArchive{name: tc.name + "/record-padded", archive: padded, names: names, contents: bodies},
		)
	}
	return out
}

func TestRoundTripGenerated(t *testing.T) {
	for _, g := range generatedArchives(t) {
		t.Run(g.name, func(t *testing.T) {
			composed, dir, idx := roundTrip(t, g.archive)
			assertIdentical(t, g.archive, composed)
			assertBlobs(t, dir, idx, g.names, g.contents)
		})
	}
}
