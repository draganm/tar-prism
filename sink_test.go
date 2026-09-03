package tarprism

import (
	"archive/tar"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lukechampine.com/blake3"
)

// memPrism holds a prism in memory: the recipe bytes, the blobs keyed by
// their Entry.Blob path, and the index. memSink fills one, memSource serves
// one.
type memPrism struct {
	recipe bytes.Buffer
	blobs  map[string][]byte
	index  *Index
}

func newMemPrism() *memPrism {
	return &memPrism{blobs: map[string][]byte{}}
}

// memSink implements Sink over a memPrism and records the calls it receives
// so tests can check DecomposeTo's protocol.
type memSink struct {
	*memPrism
	calls        []string
	recipeClosed bool
}

func newMemSink() *memSink {
	return &memSink{memPrism: newMemPrism()}
}

func (s *memSink) Recipe() (io.WriteCloser, error) {
	s.calls = append(s.calls, "recipe")
	return &memRecipeWriter{sink: s}, nil
}

func (s *memSink) Blob(index int, entry Entry, r io.Reader) error {
	s.calls = append(s.calls, fmt.Sprintf("blob:%d", index))
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.blobs[entry.Blob] = data
	return nil
}

func (s *memSink) Index(idx *Index) error {
	s.calls = append(s.calls, "index")
	s.index = idx
	return nil
}

type memRecipeWriter struct {
	sink *memSink
}

func (w *memRecipeWriter) Write(p []byte) (int, error) {
	if w.sink.recipeClosed {
		return 0, errors.New("write after close")
	}
	return w.sink.recipe.Write(p)
}

func (w *memRecipeWriter) Close() error {
	w.sink.calls = append(w.sink.calls, "close")
	w.sink.recipeClosed = true
	return nil
}

// memSource implements Source over a memPrism and counts the readers that
// ComposeFrom has not closed yet.
type memSource struct {
	*memPrism
	open int
}

func (s *memSource) Index() (*Index, error) {
	if s.index == nil {
		return nil, errors.New("memory prism has no index")
	}
	return s.index, nil
}

func (s *memSource) Recipe() (io.ReadCloser, error) {
	return s.reader(s.recipe.Bytes()), nil
}

func (s *memSource) Blob(_ int, entry Entry) (io.ReadCloser, error) {
	data, ok := s.blobs[entry.Blob]
	if !ok {
		return nil, fmt.Errorf("blob %s: %w", entry.Blob, os.ErrNotExist)
	}
	return s.reader(data), nil
}

func (s *memSource) reader(data []byte) io.ReadCloser {
	s.open++
	return &memReader{Reader: bytes.NewReader(data), src: s}
}

type memReader struct {
	*bytes.Reader
	src *memSource
}

func (r *memReader) Close() error {
	r.src.open--
	return nil
}

// memoryRoundTrip decomposes archive into a memory sink and composes it back
// from a memory source, returning the composed bytes and the prism.
func memoryRoundTrip(t *testing.T, archive []byte) ([]byte, *memPrism) {
	t.Helper()
	sink := newMemSink()
	if err := DecomposeTo(bytes.NewReader(archive), sink); err != nil {
		t.Fatalf("DecomposeTo: %v", err)
	}
	if !sink.recipeClosed {
		t.Fatal("DecomposeTo did not close the recipe writer")
	}
	src := &memSource{memPrism: sink.memPrism}
	var out bytes.Buffer
	if err := ComposeFrom(src, &out); err != nil {
		t.Fatalf("ComposeFrom: %v", err)
	}
	if src.open != 0 {
		t.Fatalf("ComposeFrom left %d readers open", src.open)
	}
	return out.Bytes(), sink.memPrism
}

// memBlob reads a blob from a memory prism, for assertEntries.
func memBlob(p *memPrism) func(Entry) ([]byte, error) {
	return func(e Entry) ([]byte, error) {
		data, ok := p.blobs[e.Blob]
		if !ok {
			return nil, fmt.Errorf("blob %s not in memory prism", e.Blob)
		}
		return data, nil
	}
}

func TestMemoryRoundTripFixtures(t *testing.T) {
	for _, fx := range archiveFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			composed, p := memoryRoundTrip(t, fx.archive)
			assertIdentical(t, fx.archive, composed)
			assertEntries(t, p.index, fx.names, fx.contents, memBlob(p))
			assertOffsets(t, p.index, fx.offsets)
			if len(p.blobs) != len(fx.names) {
				t.Errorf("memory prism holds %d blobs, want %d", len(p.blobs), len(fx.names))
			}
		})
	}
}

func TestMemoryRoundTripGenerated(t *testing.T) {
	for _, g := range generatedArchives(t) {
		t.Run(g.name, func(t *testing.T) {
			composed, p := memoryRoundTrip(t, g.archive)
			assertIdentical(t, g.archive, composed)
			assertEntries(t, p.index, g.names, g.contents, memBlob(p))
		})
	}
}

// TestMemoryMatchesDirectory checks that the memory path and the directory
// adapters produce the same recipe, blobs and index for the same archive.
func TestMemoryMatchesDirectory(t *testing.T) {
	archive := buildTar(t, tar.FormatPAX, generatedFiles(t))
	_, p := memoryRoundTrip(t, archive)
	dir := filepath.Join(t.TempDir(), "prism")
	if err := DecomposeTo(bytes.NewReader(archive), DirSink(dir)); err != nil {
		t.Fatal(err)
	}
	idx, err := ReadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(idx, p.index) {
		t.Fatalf("index differs:\ndir: %+v\nmem: %+v", idx, p.index)
	}
	recipe, err := os.ReadFile(filepath.Join(dir, RecipeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recipe, p.recipe.Bytes()) {
		t.Fatalf("recipe differs: %d bytes on disk, %d in memory", len(recipe), p.recipe.Len())
	}
	for _, e := range idx.Entries {
		disk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Blob)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(disk, p.blobs[e.Blob]) {
			t.Errorf("blob %s differs between disk and memory", e.Blob)
		}
	}
	indexFile, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeIndex(p.index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexFile, encoded) {
		t.Fatalf("recipe.json on disk differs from EncodeIndex output:\n%s\n---\n%s", indexFile, encoded)
	}
}

// TestMixedAdapters composes from a memory source what DirSink decomposed,
// and from DirSource what a memory sink decomposed.
func TestMixedAdapters(t *testing.T) {
	archive := buildTar(t, tar.FormatGNU, generatedFiles(t))

	t.Run("dir sink to memory source", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "prism")
		if err := DecomposeTo(bytes.NewReader(archive), DirSink(dir)); err != nil {
			t.Fatal(err)
		}
		p := newMemPrism()
		idx, err := ReadIndex(dir)
		if err != nil {
			t.Fatal(err)
		}
		p.index = idx
		recipe, err := os.ReadFile(filepath.Join(dir, RecipeFile))
		if err != nil {
			t.Fatal(err)
		}
		p.recipe.Write(recipe)
		for _, e := range idx.Entries {
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Blob)))
			if err != nil {
				t.Fatal(err)
			}
			p.blobs[e.Blob] = data
		}
		var out bytes.Buffer
		if err := ComposeFrom(&memSource{memPrism: p}, &out); err != nil {
			t.Fatalf("ComposeFrom: %v", err)
		}
		assertIdentical(t, archive, out.Bytes())
	})

	t.Run("memory sink to dir source", func(t *testing.T) {
		sink := newMemSink()
		if err := DecomposeTo(bytes.NewReader(archive), sink); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "prism")
		if err := os.MkdirAll(filepath.Join(dir, BlobsDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, RecipeFile), sink.recipe.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		for name, data := range sink.blobs {
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := writeIndex(dir, sink.index); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := ComposeFrom(DirSource(dir), &out); err != nil {
			t.Fatalf("ComposeFrom: %v", err)
		}
		assertIdentical(t, archive, out.Bytes())
	})
}

// twoFileArchive is a two-file archive used by the protocol and error tests.
func twoFileArchive() []byte {
	posix := "ustar\x0000"
	return concat(
		rawHeader{name: "a", typeflag: '0', size: 3, magic: posix}.block(), payload([]byte("abc")),
		rawHeader{name: "b", typeflag: '0', size: 0, magic: posix}.block(),
		endMarker)
}

func TestDecomposeToProtocol(t *testing.T) {
	archive := twoFileArchive()
	sink := newMemSink()
	if err := DecomposeTo(bytes.NewReader(archive), sink); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"recipe", "blob:0", "blob:1", "close", "index"}
	if !reflect.DeepEqual(sink.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", sink.calls, wantCalls)
	}
	want := []Entry{
		{Name: "a", Offset: 512, Size: 3, Blob: "blobs/00000001"},
		{Name: "b", Offset: 1533, Size: 0, Blob: "blobs/00000002"},
	}
	if !reflect.DeepEqual(sink.index.Entries, want) {
		t.Fatalf("entries = %+v, want %+v", sink.index.Entries, want)
	}
	if sink.index.Version != FormatVersion {
		t.Errorf("version = %d", sink.index.Version)
	}
	sum := blake3.Sum256(archive)
	if sink.index.BLAKE3 != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %s, want %s", sink.index.BLAKE3, hex.EncodeToString(sum[:]))
	}
	if got := sink.blobs["blobs/00000001"]; string(got) != "abc" {
		t.Errorf("blob 1 = %q", got)
	}
	if got, ok := sink.blobs["blobs/00000002"]; !ok || len(got) != 0 {
		t.Errorf("blob 2 = %q, %v; want an empty blob", got, ok)
	}
	wantRecipe := concat(archive[:512], make([]byte, 509), archive[1024:])
	if !bytes.Equal(sink.recipe.Bytes(), wantRecipe) {
		t.Errorf("recipe is %d bytes, want %d", sink.recipe.Len(), len(wantRecipe))
	}
}

// TestDecomposeToEntryPassedToBlob checks that Blob receives the entry with
// the same offset, size and blob name that the index records afterwards.
func TestDecomposeToEntryPassedToBlob(t *testing.T) {
	archive := buildTar(t, tar.FormatPAX, generatedFiles(t))
	var seen []Entry
	var indexes []int
	sink := &hookSink{memSink: newMemSink()}
	sink.onBlob = func(index int, entry Entry, r io.Reader) error {
		seen = append(seen, entry)
		indexes = append(indexes, index)
		return sink.memSink.Blob(index, entry, r)
	}
	if err := DecomposeTo(bytes.NewReader(archive), sink); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, sink.index.Entries) {
		t.Fatalf("entries passed to Blob:\n%+v\nindex entries:\n%+v", seen, sink.index.Entries)
	}
	for i, index := range indexes {
		if index != i {
			t.Errorf("call %d got index %d", i, index)
		}
		if seen[i].Blob != blobName(i+1) {
			t.Errorf("call %d: blob %q, want %q", i, seen[i].Blob, blobName(i+1))
		}
	}
}

// hookSink wraps a memSink and lets a test replace any Sink method.
type hookSink struct {
	*memSink
	onRecipe func() (io.WriteCloser, error)
	onBlob   func(index int, entry Entry, r io.Reader) error
	onIndex  func(idx *Index) error
}

func (s *hookSink) Recipe() (io.WriteCloser, error) {
	if s.onRecipe != nil {
		return s.onRecipe()
	}
	return s.memSink.Recipe()
}

func (s *hookSink) Blob(index int, entry Entry, r io.Reader) error {
	if s.onBlob != nil {
		return s.onBlob(index, entry, r)
	}
	return s.memSink.Blob(index, entry, r)
}

func (s *hookSink) Index(idx *Index) error {
	if s.onIndex != nil {
		return s.onIndex(idx)
	}
	return s.memSink.Index(idx)
}

// errWriteCloser fails every write, or the close, with the given errors.
type errWriteCloser struct {
	writeErr error
	closeErr error
	closed   *bool
}

func (w errWriteCloser) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(p), nil
}

func (w errWriteCloser) Close() error {
	if w.closed != nil {
		*w.closed = true
	}
	return w.closeErr
}

func TestDecomposeToShortConsumption(t *testing.T) {
	archive := twoFileArchive()
	sink := &hookSink{memSink: newMemSink()}
	sink.onBlob = func(index int, entry Entry, r io.Reader) error {
		_, err := io.CopyN(io.Discard, r, 1) // one byte of the three
		return err
	}
	err := DecomposeTo(bytes.NewReader(archive), sink)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{`entry "a"`, "sink consumed 1 of 3 bytes", "offset 0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if !sink.recipeClosed {
		t.Error("recipe writer was not closed on failure")
	}
	if sink.index != nil {
		t.Error("Index was called after a failure")
	}
}

func TestDecomposeToSinkThatReadsNothing(t *testing.T) {
	sink := &hookSink{memSink: newMemSink()}
	sink.onBlob = func(int, Entry, io.Reader) error { return nil }
	err := DecomposeTo(bytes.NewReader(twoFileArchive()), sink)
	if err == nil || !strings.Contains(err.Error(), "sink consumed 0 of 3 bytes") {
		t.Fatalf("error = %v, want 'sink consumed 0 of 3 bytes'", err)
	}
}

// TestDecomposeToTruncatedContent checks that a memory sink that reads to EOF
// still gets the truncated-content error, not a short-consumption one.
func TestDecomposeToTruncatedContent(t *testing.T) {
	regB := rawHeader{name: "b", typeflag: '0', size: 3, magic: "ustar\x0000"}.block()
	sink := newMemSink()
	err := DecomposeTo(bytes.NewReader(concat(regB, []byte("ab"))), sink)
	if err == nil || !strings.Contains(err.Error(), "truncated content (2 of 3 bytes)") {
		t.Fatalf("error = %v, want 'truncated content (2 of 3 bytes)'", err)
	}
	if strings.Contains(err.Error(), "sink consumed") {
		t.Fatalf("error = %v, want no short-consumption label", err)
	}
	if !sink.recipeClosed {
		t.Error("recipe writer was not closed on failure")
	}
}

// TestDecomposeToWrapsContentReadError checks that a genuine I/O failure in
// the middle of file content is reported as such, wrapped with %w, even
// though the sink swallowed it.
func TestDecomposeToWrapsContentReadError(t *testing.T) {
	regB := rawHeader{name: "b", typeflag: '0', size: 3, magic: "ustar\x0000"}.block()
	r := io.MultiReader(bytes.NewReader(concat(regB, []byte("a"))), errReader{})
	sink := &hookSink{memSink: newMemSink()}
	sink.onBlob = func(_ int, _ Entry, r io.Reader) error {
		io.Copy(io.Discard, r) // ignore the error, like a careless sink
		return nil
	}
	err := DecomposeTo(r, sink)
	if !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want wrapping %v", err, errBoom)
	}
	if strings.Contains(err.Error(), "truncated") || strings.Contains(err.Error(), "consumed") {
		t.Fatalf("error = %v, want no truncation or consumption label", err)
	}
}

func TestDecomposeToSinkErrors(t *testing.T) {
	archive := twoFileArchive()
	errRecipe := errors.New("recipe failed")
	errBlob := errors.New("blob failed")
	errIndex := errors.New("index failed")
	errWrite := errors.New("write failed")
	errClose := errors.New("close failed")

	t.Run("recipe", func(t *testing.T) {
		sink := &hookSink{memSink: newMemSink()}
		sink.onRecipe = func() (io.WriteCloser, error) { return nil, errRecipe }
		if err := DecomposeTo(bytes.NewReader(archive), sink); !errors.Is(err, errRecipe) {
			t.Fatalf("error = %v, want %v", err, errRecipe)
		}
		if len(sink.calls) != 0 {
			t.Errorf("calls after a Recipe failure: %v", sink.calls)
		}
	})
	t.Run("blob", func(t *testing.T) {
		sink := &hookSink{memSink: newMemSink()}
		sink.onBlob = func(int, Entry, io.Reader) error { return errBlob }
		err := DecomposeTo(bytes.NewReader(archive), sink)
		if !errors.Is(err, errBlob) {
			t.Fatalf("error = %v, want wrapping %v", err, errBlob)
		}
		if !strings.Contains(err.Error(), `entry "a"`) {
			t.Errorf("error = %v, want the entry name", err)
		}
		if !sink.recipeClosed {
			t.Error("recipe writer was not closed on failure")
		}
		if sink.index != nil {
			t.Error("Index was called after a failure")
		}
	})
	t.Run("index", func(t *testing.T) {
		sink := &hookSink{memSink: newMemSink()}
		sink.onIndex = func(*Index) error { return errIndex }
		if err := DecomposeTo(bytes.NewReader(archive), sink); !errors.Is(err, errIndex) {
			t.Fatalf("error = %v, want %v", err, errIndex)
		}
		if !sink.recipeClosed {
			t.Error("recipe writer was not closed before Index")
		}
	})
	t.Run("recipe write", func(t *testing.T) {
		closed := false
		sink := &hookSink{memSink: newMemSink()}
		sink.onRecipe = func() (io.WriteCloser, error) {
			return errWriteCloser{writeErr: errWrite, closed: &closed}, nil
		}
		err := DecomposeTo(bytes.NewReader(archive), sink)
		if !errors.Is(err, errWrite) {
			t.Fatalf("error = %v, want wrapping %v", err, errWrite)
		}
		if !closed {
			t.Error("recipe writer was not closed on failure")
		}
		if sink.index != nil {
			t.Error("Index was called after a failure")
		}
	})
	t.Run("recipe close", func(t *testing.T) {
		sink := &hookSink{memSink: newMemSink()}
		sink.onRecipe = func() (io.WriteCloser, error) {
			return errWriteCloser{closeErr: errClose}, nil
		}
		err := DecomposeTo(bytes.NewReader(archive), sink)
		if !errors.Is(err, errClose) {
			t.Fatalf("error = %v, want wrapping %v", err, errClose)
		}
		if sink.index != nil {
			t.Error("Index was called after a failure")
		}
	})
}

// decomposed returns a memory prism for the two-file archive.
func decomposed(t *testing.T) (*memPrism, []byte) {
	t.Helper()
	archive := twoFileArchive()
	sink := newMemSink()
	if err := DecomposeTo(bytes.NewReader(archive), sink); err != nil {
		t.Fatal(err)
	}
	return sink.memPrism, archive
}

// hookSource wraps a memSource and lets a test replace any Source method.
type hookSource struct {
	*memSource
	onIndex  func() (*Index, error)
	onRecipe func() (io.ReadCloser, error)
	onBlob   func(index int, entry Entry) (io.ReadCloser, error)
}

func (s *hookSource) Index() (*Index, error) {
	if s.onIndex != nil {
		return s.onIndex()
	}
	return s.memSource.Index()
}

func (s *hookSource) Recipe() (io.ReadCloser, error) {
	if s.onRecipe != nil {
		return s.onRecipe()
	}
	return s.memSource.Recipe()
}

func (s *hookSource) Blob(index int, entry Entry) (io.ReadCloser, error) {
	if s.onBlob != nil {
		return s.onBlob(index, entry)
	}
	return s.memSource.Blob(index, entry)
}

func TestComposeFromBlobLength(t *testing.T) {
	cases := []struct {
		name    string
		blob    []byte
		wantErr string
	}{
		{"short", []byte("ab"), "blob blobs/00000001 ends after 2 of 3 bytes"},
		{"empty", nil, "blob blobs/00000001 ends after 0 of 3 bytes"},
		{"long", []byte("abcd"), "blob blobs/00000001 is longer than the 3 bytes"},
		{"tampered", []byte("xyz"), "digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := decomposed(t)
			p.blobs["blobs/00000001"] = tc.blob
			src := &memSource{memPrism: p}
			err := ComposeFrom(src, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "entry 0 (a)") && tc.name != "tampered" {
				t.Errorf("error = %v, want the entry number and name", err)
			}
			if src.open != 0 {
				t.Errorf("%d readers left open", src.open)
			}
		})
	}
}

func TestComposeFromSourceErrors(t *testing.T) {
	errIndex := errors.New("index failed")
	errRecipe := errors.New("recipe failed")
	errBlob := errors.New("blob failed")

	t.Run("index error", func(t *testing.T) {
		p, _ := decomposed(t)
		src := &hookSource{memSource: &memSource{memPrism: p}}
		src.onIndex = func() (*Index, error) { return nil, errIndex }
		if err := ComposeFrom(src, io.Discard); !errors.Is(err, errIndex) {
			t.Fatalf("error = %v, want %v", err, errIndex)
		}
	})
	t.Run("nil index", func(t *testing.T) {
		p, _ := decomposed(t)
		src := &hookSource{memSource: &memSource{memPrism: p}}
		src.onIndex = func() (*Index, error) { return nil, nil }
		if err := ComposeFrom(src, io.Discard); err == nil || !strings.Contains(err.Error(), "no index") {
			t.Fatalf("error = %v, want 'no index'", err)
		}
	})
	t.Run("invalid index is rejected", func(t *testing.T) {
		p, _ := decomposed(t)
		p.index.Version = 2
		err := ComposeFrom(&memSource{memPrism: p}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "unsupported version 2") {
			t.Fatalf("error = %v, want 'unsupported version 2'", err)
		}
	})
	t.Run("missing blob", func(t *testing.T) {
		p, _ := decomposed(t)
		delete(p.blobs, "blobs/00000002")
		err := ComposeFrom(&memSource{memPrism: p}, io.Discard)
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "entry 1 (b)") {
			t.Fatalf("error = %v, want ErrNotExist for entry 1 (b)", err)
		}
	})
	t.Run("recipe error", func(t *testing.T) {
		p, _ := decomposed(t)
		src := &hookSource{memSource: &memSource{memPrism: p}}
		src.onRecipe = func() (io.ReadCloser, error) { return nil, errRecipe }
		if err := ComposeFrom(src, io.Discard); !errors.Is(err, errRecipe) {
			t.Fatalf("error = %v, want %v", err, errRecipe)
		}
	})
	t.Run("blob error", func(t *testing.T) {
		p, _ := decomposed(t)
		src := &hookSource{memSource: &memSource{memPrism: p}}
		src.onBlob = func(int, Entry) (io.ReadCloser, error) { return nil, errBlob }
		err := ComposeFrom(src, io.Discard)
		if !errors.Is(err, errBlob) || !strings.Contains(err.Error(), "entry 0 (a)") {
			t.Fatalf("error = %v, want wrapping %v for entry 0 (a)", err, errBlob)
		}
	})
	t.Run("blob read error", func(t *testing.T) {
		p, _ := decomposed(t)
		src := &hookSource{memSource: &memSource{memPrism: p}}
		src.onBlob = func(int, Entry) (io.ReadCloser, error) { return io.NopCloser(errReader{}), nil }
		err := ComposeFrom(src, io.Discard)
		if !errors.Is(err, errBoom) || strings.Contains(err.Error(), "ends after") {
			t.Fatalf("error = %v, want wrapping %v without a truncation label", err, errBoom)
		}
	})
	t.Run("short recipe", func(t *testing.T) {
		p, _ := decomposed(t)
		p.recipe.Truncate(600)
		err := ComposeFrom(&memSource{memPrism: p}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "recipe ends at byte 600, splice point is 1533") {
			t.Fatalf("error = %v, want 'recipe ends at byte 600, splice point is 1533'", err)
		}
	})
	t.Run("tampered recipe", func(t *testing.T) {
		p, _ := decomposed(t)
		p.recipe.Bytes()[0] = 'z'
		err := ComposeFrom(&memSource{memPrism: p}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "digest") {
			t.Fatalf("error = %v, want a digest mismatch", err)
		}
	})
}

// TestComposeFromOutputMatchesInput checks that composing the memory prism of
// the two-file archive reproduces the archive.
func TestComposeFromOutputMatchesInput(t *testing.T) {
	p, archive := decomposed(t)
	var out bytes.Buffer
	if err := ComposeFrom(&memSource{memPrism: p}, &out); err != nil {
		t.Fatal(err)
	}
	assertIdentical(t, archive, out.Bytes())
}
