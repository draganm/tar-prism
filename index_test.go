package tarprism

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validDigest = "0000000000000000000000000000000000000000000000000000000000000000"

func writeTestIndex(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, IndexFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadIndex(t *testing.T) {
	head := `{"version":1,"blake3":"` + validDigest + `",`
	tests := []struct{ name, body, wantErr string }{
		{"valid", head + `"entries":[{"name":"a","offset":512,"size":3,"blob":"blobs/00000001"},{"name":"b","offset":1536,"size":0,"blob":"blobs/00000002"}]}`, ""},
		{"no entries", head + `"entries":[]}`, ""},
		{"wrong version", `{"version":2,"blake3":"` + validDigest + `","entries":[]}`, "unsupported version 2"},
		{"short digest", `{"version":1,"blake3":"abc","entries":[]}`, "64 hex"},
		{"non-increasing offset", head + `"entries":[{"offset":512,"size":1,"blob":"blobs/00000001"},{"offset":512,"size":1,"blob":"blobs/00000002"}]}`, "does not increase"},
		{"escaping blob path", head + `"entries":[{"offset":512,"size":1,"blob":"../x"}]}`, "inside the prism directory"},
		{"empty blob path", head + `"entries":[{"offset":512,"size":1,"blob":""}]}`, "inside the prism directory"},
		{"negative size", head + `"entries":[{"offset":512,"size":-1,"blob":"blobs/00000001"}]}`, "negative"},
		{"not json", `{`, "parsing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestIndex(t, dir, tc.body)
			idx, err := ReadIndex(dir)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if idx.Version != 1 || idx.BLAKE3 != validDigest {
					t.Fatalf("index = %+v", idx)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
			// DecodeIndex is the same validation over bytes.
			if _, err := DecodeIndex([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DecodeIndex error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestReadIndexMissing(t *testing.T) {
	_, err := ReadIndex(t.TempDir())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want ErrNotExist", err)
	}
}

func TestWriteIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &Index{Version: FormatVersion, BLAKE3: validDigest, Entries: []Entry{{Name: "a", Offset: 512, Size: 3, Blob: "blobs/00000001"}}}
	if err := writeIndex(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0] != want.Entries[0] || got.BLAKE3 != want.BLAKE3 {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestEncodeDecodeIndex checks that EncodeIndex produces exactly what
// Decompose writes to recipe.json (indented JSON plus a newline) and that
// DecodeIndex reads it back.
func TestEncodeDecodeIndex(t *testing.T) {
	want := &Index{Version: FormatVersion, BLAKE3: validDigest, Entries: []Entry{
		{Name: "a", Offset: 512, Size: 3, Blob: "blobs/00000001"},
		{Name: "b", Offset: 1536, Size: 0, Blob: "blobs/00000002"},
	}}
	data, err := EncodeIndex(want)
	if err != nil {
		t.Fatal(err)
	}
	indented, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, append(indented, '\n')) {
		t.Fatalf("EncodeIndex = %q, want indented JSON plus newline", data)
	}
	got, err := DecodeIndex(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DecodeIndex = %+v, want %+v", got, want)
	}

	dir := t.TempDir()
	if err := writeIndex(dir, want); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, data) {
		t.Fatalf("recipe.json on disk %q differs from EncodeIndex %q", onDisk, data)
	}
}

func TestBlobName(t *testing.T) {
	if got := blobName(1); got != "blobs/00000001" {
		t.Errorf("blobName(1) = %q", got)
	}
	if got := blobName(123456789); got != "blobs/123456789" {
		t.Errorf("blobName(123456789) = %q", got)
	}
}
