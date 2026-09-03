package tarprism

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Sink receives the parts of a decomposed archive from DecomposeTo.
//
// Recipe is called once before anything else, Blob once per regular file in
// archive order, and Index once at the end. DecomposeTo writes the recipe
// throughout and closes the recipe writer after its last byte, before Index
// is called; when decomposition fails it closes the recipe writer as well and
// does not call Index.
type Sink interface {
	// Recipe returns the writer that receives recipe.bin.
	Recipe() (io.WriteCloser, error)
	// Blob receives the content of the regular file described by entry.
	// index is 0-based and matches Index.Entries[index]; entry.Blob carries
	// the blobs/%08d name a prism directory would use. r yields exactly
	// entry.Size bytes and is valid only until Blob returns. Blob must
	// consume all of them: DecomposeTo reports an error if it consumes
	// fewer.
	Blob(index int, entry Entry, r io.Reader) error
	// Index receives the complete index once every blob has been delivered.
	// The sink owns idx after the call.
	Index(idx *Index) error
}

// DirSink returns a Sink that writes a prism directory at dir: recipe.bin,
// blobs/, and recipe.json. dir must not exist or must be empty; it is
// created, together with blobs/, when Recipe is called.
func DirSink(dir string) Sink {
	return dirSink{dir: dir}
}

type dirSink struct {
	dir string
}

func (s dirSink) Recipe() (io.WriteCloser, error) {
	if err := prepareDir(s.dir); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(s.dir, RecipeFile))
	if err != nil {
		return nil, fmt.Errorf("creating recipe: %w", err)
	}
	return f, nil
}

func (s dirSink) Blob(_ int, entry Entry, r io.Reader) error {
	f, err := os.Create(filepath.Join(s.dir, filepath.FromSlash(entry.Blob)))
	if err != nil {
		return fmt.Errorf("creating blob %s: %w", entry.Blob, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("writing blob %s: %w", entry.Blob, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing blob %s: %w", entry.Blob, err)
	}
	return nil
}

func (s dirSink) Index(idx *Index) error {
	return writeIndex(s.dir, idx)
}

// prepareDir creates dir and its blobs subdirectory. dir may already exist
// only if it is empty.
func prepareDir(dir string) error {
	existing, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("checking %s: %w", dir, err)
	case len(existing) > 0:
		return fmt.Errorf("%s exists and is not empty", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, BlobsDir), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return nil
}
