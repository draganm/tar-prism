package tarprism

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Source serves the parts of a prism to ComposeFrom.
//
// ComposeFrom calls Index once, then Recipe once, then Blob once per entry in
// index order while the recipe is being copied. Every reader it receives is
// closed by ComposeFrom.
type Source interface {
	// Index returns the prism's index. ComposeFrom validates it.
	Index() (*Index, error)
	// Recipe returns a reader over recipe.bin.
	Recipe() (io.ReadCloser, error)
	// Blob returns a reader over the content of Index.Entries[index], which
	// is entry. The reader must yield exactly entry.Size bytes: ComposeFrom
	// fails if it ends early or has more.
	Blob(index int, entry Entry) (io.ReadCloser, error)
}

// DirSource returns a Source that reads the prism directory at dir.
func DirSource(dir string) Source {
	return dirSource{dir: dir}
}

type dirSource struct {
	dir string
}

func (s dirSource) Index() (*Index, error) {
	return ReadIndex(s.dir)
}

func (s dirSource) Recipe() (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(s.dir, RecipeFile))
	if err != nil {
		return nil, fmt.Errorf("opening recipe: %w", err)
	}
	return f, nil
}

func (s dirSource) Blob(_ int, entry Entry) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(s.dir, filepath.FromSlash(entry.Blob)))
	if err != nil {
		return nil, fmt.Errorf("opening blob %s: %w", entry.Blob, err)
	}
	return f, nil
}
