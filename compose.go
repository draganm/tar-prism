package tarprysm

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"lukechampine.com/blake3"
)

// Compose reads the prysm in dir and writes the original archive to w. The
// output is verified against the BLAKE3 digest recorded at decompose time;
// on a mismatch the (already written) output must not be trusted.
func Compose(dir string, w io.Writer) error {
	idx, err := ReadIndex(dir)
	if err != nil {
		return err
	}
	recipeFile, err := os.Open(filepath.Join(dir, RecipeFile))
	if err != nil {
		return fmt.Errorf("opening recipe: %w", err)
	}
	defer recipeFile.Close()

	hasher := blake3.New(32, nil)
	out := bufio.NewWriterSize(io.MultiWriter(w, hasher), bufSize)
	recipe := bufio.NewReaderSize(recipeFile, bufSize)
	var pos int64
	for i, e := range idx.Entries {
		n, err := io.CopyN(out, recipe, e.Offset-pos)
		pos += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("entry %d (%s): recipe ends at byte %d, splice point is %d", i, e.Name, pos, e.Offset)
			}
			return fmt.Errorf("writing output: %w", err)
		}
		if err := copyBlob(out, dir, i, e); err != nil {
			return err
		}
	}
	if _, err := io.Copy(out, recipe); err != nil {
		return fmt.Errorf("copying recipe: %w", err)
	}
	if err := out.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != idx.BLAKE3 {
		return fmt.Errorf("composed archive digest %s does not match recorded %s", got, idx.BLAKE3)
	}
	return nil
}

// copyBlob writes entry i's blob to out after checking its size matches the
// index.
func copyBlob(out io.Writer, dir string, i int, e Entry) error {
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(e.Blob)))
	if err != nil {
		return fmt.Errorf("entry %d (%s): %w", i, e.Name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("entry %d (%s): %w", i, e.Name, err)
	}
	if info.Size() != e.Size {
		return fmt.Errorf("entry %d (%s): blob %s is %d bytes, index says %d", i, e.Name, e.Blob, info.Size(), e.Size)
	}
	n, err := io.CopyN(out, f, e.Size)
	if err != nil {
		return fmt.Errorf("entry %d (%s): reading blob %s: %w after %d of %d bytes", i, e.Name, e.Blob, err, n, e.Size)
	}
	return nil
}
