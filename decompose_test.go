package tarprism

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lukechampine.com/blake3"
)

// errBoom is a sentinel non-EOF read error used to prove that decompose
// wraps genuine I/O failures instead of mislabelling them as truncation.
var errBoom = errors.New("boom")

// errReader always fails with errBoom.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) {
	return 0, errBoom
}

func TestDecomposeSingleFile(t *testing.T) {
	hdr := rawHeader{name: "a.txt", typeflag: '0', size: 5, magic: "ustar\x0000"}.block()
	archive := concat(hdr, payload([]byte("hello")), endMarker)
	dir := filepath.Join(t.TempDir(), "prysm")
	if err := Decompose(bytes.NewReader(archive), dir); err != nil {
		t.Fatalf("Decompose: %v", err)
	}

	recipe, err := os.ReadFile(filepath.Join(dir, RecipeFile))
	if err != nil {
		t.Fatal(err)
	}
	wantRecipe := concat(hdr, make([]byte, 507), endMarker)
	if !bytes.Equal(recipe, wantRecipe) {
		t.Errorf("recipe.bin is %d bytes, want %d (header, 507 padding bytes, end marker)", len(recipe), len(wantRecipe))
	}

	blob, err := os.ReadFile(filepath.Join(dir, BlobsDir, "00000001"))
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "hello" {
		t.Errorf("blob = %q, want %q", blob, "hello")
	}

	idx, err := ReadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{Name: "a.txt", Offset: 512, Size: 5, Blob: "blobs/00000001"}}
	if !reflect.DeepEqual(idx.Entries, want) {
		t.Errorf("entries = %+v, want %+v", idx.Entries, want)
	}
	sum := blake3.Sum256(archive)
	if idx.BLAKE3 != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %s, want %s", idx.BLAKE3, hex.EncodeToString(sum[:]))
	}
}

func TestDecomposeEmptyFileGetsBlob(t *testing.T) {
	archive := concat(rawHeader{name: "empty", typeflag: '0', size: 0, magic: "ustar\x0000"}.block(), endMarker)
	dir := filepath.Join(t.TempDir(), "prysm")
	if err := Decompose(bytes.NewReader(archive), dir); err != nil {
		t.Fatal(err)
	}
	idx, err := ReadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 1 || idx.Entries[0].Size != 0 || idx.Entries[0].Offset != 512 {
		t.Fatalf("entries = %+v", idx.Entries)
	}
	if info, err := os.Stat(filepath.Join(dir, BlobsDir, "00000001")); err != nil || info.Size() != 0 {
		t.Fatalf("empty blob: %v, %v", info, err)
	}
}

// TestDecomposeWrapsReadError proves that a genuine I/O failure while
// reading a meta entry is wrapped with %w rather than reported as
// "truncated", so it can be found with errors.Is.
func TestDecomposeWrapsReadError(t *testing.T) {
	xHeaderBlock := rawHeader{name: "x", typeflag: 'x', size: 10}.block()
	r := io.MultiReader(bytes.NewReader(xHeaderBlock), errReader{})
	dir := filepath.Join(t.TempDir(), "prysm")
	err := Decompose(r, dir)
	if !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want wrapping %v", err, errBoom)
	}
	if strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want no false 'truncated' label", err)
	}
}

func TestDecomposeTargetDir(t *testing.T) {
	archive := concat(rawHeader{name: "b", typeflag: '0', size: 3, magic: "ustar\x0000"}.block(), payload([]byte("abc")), endMarker)

	t.Run("non-empty directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		err := Decompose(bytes.NewReader(archive), dir)
		if err == nil || !strings.Contains(err.Error(), "not empty") {
			t.Fatalf("error = %v, want 'not empty'", err)
		}
	})
	t.Run("existing empty directory", func(t *testing.T) {
		if err := Decompose(bytes.NewReader(archive), t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("path is a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Decompose(bytes.NewReader(archive), path); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("nested path is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "b", "prysm")
		if err := Decompose(bytes.NewReader(archive), dir); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{RecipeFile, IndexFile, BlobsDir} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Error(err)
			}
		}
	})
}
