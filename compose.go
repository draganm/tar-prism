package tarprism

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"lukechampine.com/blake3"
)

// Compose reads the prism in dir and writes the original archive to w. The
// output is verified against the BLAKE3 digest recorded at decompose time;
// on a mismatch the (already written) output must not be trusted.
//
// It is ComposeFrom(DirSource(dir), w).
func Compose(dir string, w io.Writer) error {
	return ComposeFrom(DirSource(dir), w)
}

// ComposeFrom writes the archive described by src to w: the recipe with each
// blob spliced in at its offset. Exactly entry.Size bytes are copied from
// every blob reader, and a reader that ends early or has more bytes is an
// error. The output is verified against the index's BLAKE3 digest; on a
// mismatch the (already written) output must not be trusted.
func ComposeFrom(src Source, w io.Writer) error {
	idx, err := src.Index()
	if err != nil {
		return err
	}
	if idx == nil {
		return errors.New("source returned no index")
	}
	if err := idx.validate(); err != nil {
		return fmt.Errorf("invalid index: %w", err)
	}
	recipeReader, err := src.Recipe()
	if err != nil {
		return err
	}
	defer recipeReader.Close()

	hasher := blake3.New(32, nil)
	out := bufio.NewWriterSize(io.MultiWriter(w, hasher), bufSize)
	recipe := bufio.NewReaderSize(recipeReader, bufSize)
	var pos int64
	for i, e := range idx.Entries {
		n, err := io.CopyN(out, recipe, e.Offset-pos)
		pos += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("entry %d (%s): recipe ends at byte %d, splice point is %d", i, e.Name, pos, e.Offset)
			}
			return fmt.Errorf("copying recipe: %w", err)
		}
		if err := copyBlob(out, src, i, e); err != nil {
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

// copyBlob writes exactly e.Size bytes of entry i's blob to out and fails if
// the source's reader has fewer or more.
func copyBlob(out io.Writer, src Source, i int, e Entry) error {
	rc, err := src.Blob(i, e)
	if err != nil {
		return fmt.Errorf("entry %d (%s): %w", i, e.Name, err)
	}
	defer rc.Close()
	n, err := io.CopyN(out, rc, e.Size)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("entry %d (%s): blob %s ends after %d of %d bytes", i, e.Name, e.Blob, n, e.Size)
		}
		return fmt.Errorf("entry %d (%s): reading blob %s: %w", i, e.Name, e.Blob, err)
	}
	var extra [1]byte
	switch _, err := io.ReadFull(rc, extra[:]); {
	case err == nil:
		return fmt.Errorf("entry %d (%s): blob %s is longer than the %d bytes recorded in the index", i, e.Name, e.Blob, e.Size)
	case errors.Is(err, io.EOF):
		return nil
	default:
		return fmt.Errorf("entry %d (%s): reading blob %s: %w", i, e.Name, e.Blob, err)
	}
}
