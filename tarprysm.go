// Package tarprysm splits an uncompressed tar archive into a recipe (every
// byte that is not regular-file content, kept verbatim) and numbered blobs
// (the file contents), and reassembles the byte-identical archive from them.
package tarprysm

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Files and directories that make up a prysm directory.
const (
	RecipeFile = "recipe.bin"
	IndexFile  = "recipe.json"
	BlobsDir   = "blobs"
)

// FormatVersion is the recipe.json version written by Decompose and accepted
// by Compose.
const FormatVersion = 1

// Index is the content of recipe.json: where each blob is spliced back into
// the recipe, and the BLAKE3 digest of the original archive.
type Index struct {
	Version int     `json:"version"`
	BLAKE3  string  `json:"blake3"`
	Entries []Entry `json:"entries"`
}

// Entry describes one regular-file blob.
type Entry struct {
	// Name is the entry's name in the archive, for human readers only.
	Name string `json:"name"`
	// Offset is the byte position in recipe.bin where the blob is spliced in.
	Offset int64 `json:"offset"`
	// Size is the blob's length in bytes.
	Size int64 `json:"size"`
	// Blob is the blob's path relative to the prysm directory, slash-separated.
	Blob string `json:"blob"`
}

// ReadIndex parses and validates <dir>/recipe.json.
func ReadIndex(dir string) (*Index, error) {
	data, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", IndexFile, err)
	}
	if err := idx.validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", IndexFile, err)
	}
	return &idx, nil
}

func (idx *Index) validate() error {
	if idx.Version != FormatVersion {
		return fmt.Errorf("unsupported version %d (want %d)", idx.Version, FormatVersion)
	}
	if digest, err := hex.DecodeString(idx.BLAKE3); err != nil || len(digest) != 32 {
		return errors.New("blake3 must be 64 hex characters")
	}
	prev := int64(-1)
	for i, e := range idx.Entries {
		switch {
		case e.Offset < 0 || e.Size < 0:
			return fmt.Errorf("entry %d: negative offset or size", i)
		case e.Offset <= prev:
			return fmt.Errorf("entry %d: offset %d does not increase (previous %d)", i, e.Offset, prev)
		case e.Blob == "" || !filepath.IsLocal(filepath.FromSlash(e.Blob)):
			return fmt.Errorf("entry %d: blob path %q must be inside the prysm directory", i, e.Blob)
		}
		prev = e.Offset
	}
	return nil
}

func writeIndex(dir string, idx *Index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, IndexFile), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}
	return nil
}

// blobName returns the slash-separated relative path of the n-th (1-based)
// blob.
func blobName(n int) string {
	return fmt.Sprintf("%s/%08d", BlobsDir, n)
}
