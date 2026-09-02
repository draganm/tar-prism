package tarprysm

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// roundTrip decomposes archive into a fresh prysm directory and composes it
// back, returning the composed bytes, the prysm directory, and its index.
func roundTrip(t *testing.T, archive []byte) ([]byte, string, *Index) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "prysm")
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
// and that each blob holds the corresponding content.
func assertBlobs(t *testing.T, dir string, idx *Index, names []string, contents [][]byte) {
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
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Blob)))
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

func TestRoundTripGenerated(t *testing.T) {
	big := randomBytes(3<<20 + 17)
	prefixName := strings.Repeat("d", 120) + "/file.txt" // needs the ustar prefix field
	deepName := strings.Repeat("d/", 140) + "leaf.txt"   // > 255 bytes: needs PAX path or GNU 'L'
	longLink := strings.Repeat("t", 150)                 // > 100 bytes: needs PAX linkpath or GNU 'K'

	// Fits every format, including ustar's prefix/name split.
	common := []testFile{
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
	// Needs PAX records or GNU long-name entries.
	extended := append(append([]testFile{}, common...),
		testFile{hdr: tar.Header{Name: deepName, Typeflag: tar.TypeReg}, body: []byte("deep\n")},
		testFile{hdr: tar.Header{Name: "longlink", Typeflag: tar.TypeSymlink, Linkname: longLink}},
		testFile{hdr: tar.Header{Name: "after.txt", Typeflag: tar.TypeReg}, body: []byte("after\n")},
	)

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
		{"ustar", tar.FormatUSTAR, common},
		{"pax", tar.FormatPAX, extended},
		{"gnu", tar.FormatGNU, extended},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := buildTar(t, tc.format, tc.files)
			composed, dir, idx := roundTrip(t, archive)
			assertIdentical(t, archive, composed)
			names, bodies := regularFiles(tc.files)
			assertBlobs(t, dir, idx, names, bodies)
		})
		t.Run(tc.name+"/record-padded", func(t *testing.T) {
			archive := buildTar(t, tc.format, tc.files)
			padded := append(archive, make([]byte, 10240-len(archive)%10240)...)
			composed, _, _ := roundTrip(t, padded)
			assertIdentical(t, padded, composed)
		})
	}
}
