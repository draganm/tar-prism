package tarprysm

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"lukechampine.com/blake3"
)

// maxMetaSize bounds the payload of PAX and GNU long-name entries, which are
// held in memory while they are parsed.
const maxMetaSize = 256 << 20

const bufSize = 1 << 20

// Decompose reads an uncompressed tar archive from r and writes a prysm into
// dir: recipe.bin, recipe.json, and blobs/. dir must not exist or must be
// empty. On error the partially written directory is left in place.
func Decompose(r io.Reader, dir string) error {
	if err := prepareDir(dir); err != nil {
		return err
	}
	recipeFile, err := os.Create(filepath.Join(dir, RecipeFile))
	if err != nil {
		return fmt.Errorf("creating recipe: %w", err)
	}
	defer recipeFile.Close()

	hasher := blake3.New(32, nil)
	d := &decomposer{
		in:      bufio.NewReaderSize(io.TeeReader(r, hasher), bufSize),
		recipe:  bufio.NewWriterSize(recipeFile, bufSize),
		dir:     dir,
		entries: []Entry{},
	}
	if err := d.run(); err != nil {
		return err
	}
	if err := d.recipe.Flush(); err != nil {
		return fmt.Errorf("writing recipe: %w", err)
	}
	if err := recipeFile.Close(); err != nil {
		return fmt.Errorf("writing recipe: %w", err)
	}
	return writeIndex(dir, &Index{
		Version: FormatVersion,
		BLAKE3:  hex.EncodeToString(hasher.Sum(nil)),
		Entries: d.entries,
	})
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

// decomposer walks the archive block by block, sending regular-file content
// to blobs and everything else to the recipe.
type decomposer struct {
	in      *bufio.Reader
	recipe  *bufio.Writer
	dir     string
	inPos   int64 // bytes consumed from the archive, for error messages
	outPos  int64 // bytes written to the recipe
	entries []Entry

	// State carried from PAX 'x' and GNU 'L' entries to the next non-meta
	// entry.
	paxSize    int64
	hasPAXSize bool
	paxPath    string
	gnuName    string
}

func (d *decomposer) run() error {
	var blk [blockSize]byte
	for {
		hdrPos := d.inPos
		n, err := io.ReadFull(d.in, blk[:])
		switch {
		case err == io.EOF:
			return nil // archive without an end-of-archive marker
		case err == io.ErrUnexpectedEOF:
			return fmt.Errorf("offset %d: truncated header (%d of %d bytes)", hdrPos, n, blockSize)
		case err != nil:
			return fmt.Errorf("offset %d: reading header: %w", hdrPos, err)
		}
		d.inPos += blockSize
		if isZeroBlock(blk[:]) {
			return d.copyTail(blk[:])
		}
		if !checksumOK(blk[:]) {
			return fmt.Errorf("offset %d: invalid header checksum", hdrPos)
		}
		h, err := parseHeader(blk[:])
		if err != nil {
			return fmt.Errorf("offset %d: %w", hdrPos, err)
		}
		if err := d.writeRecipe(blk[:]); err != nil {
			return err
		}
		if err := d.handle(h, hdrPos); err != nil {
			return err
		}
	}
}

// copyTail writes the end-of-archive block and everything after it to the
// recipe verbatim.
func (d *decomposer) copyTail(blk []byte) error {
	if err := d.writeRecipe(blk); err != nil {
		return err
	}
	n, err := io.Copy(d.recipe, d.in)
	d.inPos += n
	d.outPos += n
	if err != nil {
		return fmt.Errorf("offset %d: copying archive tail: %w", d.inPos, err)
	}
	return nil
}

func (d *decomposer) handle(h header, hdrPos int64) error {
	switch h.typeflag {
	case typeXHeader:
		payload, err := d.readMeta(h.size, hdrPos)
		if err != nil {
			return err
		}
		records, err := parsePAX(payload)
		if err != nil {
			return fmt.Errorf("offset %d: %w", hdrPos, err)
		}
		if v, ok := records["size"]; ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || n > math.MaxInt64-blockSize {
				return fmt.Errorf("offset %d: invalid pax size %q", hdrPos, v)
			}
			d.paxSize, d.hasPAXSize = n, true
		}
		if v, ok := records["path"]; ok {
			d.paxPath = v
		}
		return nil
	case typeGNULongName:
		payload, err := d.readMeta(h.size, hdrPos)
		if err != nil {
			return err
		}
		d.gnuName = cstring(payload)
		return nil
	case typeXGlobal, typeGNULongLink:
		return d.copyPayload(h.size, hdrPos)
	}

	// A non-meta entry consumes the pending PAX and GNU state.
	size, paxSized := h.size, d.hasPAXSize
	if paxSized {
		size = d.paxSize
	}
	name := h.name
	switch {
	case d.gnuName != "":
		name = d.gnuName
	case d.paxPath != "":
		name = d.paxPath
	}
	d.paxSize, d.hasPAXSize, d.paxPath, d.gnuName = 0, false, "", ""

	isDir := h.typeflag == typeRegA && strings.HasSuffix(name, "/")
	switch {
	case h.typeflag == typeReg || h.typeflag == typeCont || (h.typeflag == typeRegA && !isDir):
		return d.extract(name, size, hdrPos)
	case h.typeflag == typeGNUSparse:
		if err := d.copySparseExtensions(h.isExtended); err != nil {
			return err
		}
		return d.extract(name, size, hdrPos)
	case isDir || (h.typeflag >= typeLink && h.typeflag <= typeFifo):
		// Header-only types. A nonzero size is usually a writer quirk, but
		// PAX permits real payloads on hard links: obey an explicit PAX size,
		// otherwise trust the size only if what follows cannot be a header.
		if size == 0 || (!paxSized && d.nextIsHeader()) {
			return nil
		}
		return d.copyPayload(size, hdrPos)
	default:
		return d.copyPayload(size, hdrPos)
	}
}

// copySparseExtensions copies old GNU sparse extension blocks to the recipe
// while their "is extended" flag says another one follows.
func (d *decomposer) copySparseExtensions(extended bool) error {
	var blk [blockSize]byte
	for extended {
		if _, err := io.ReadFull(d.in, blk[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("offset %d: truncated sparse extension block", d.inPos)
			}
			return fmt.Errorf("offset %d: reading sparse extension block: %w", d.inPos, err)
		}
		d.inPos += blockSize
		if err := d.writeRecipe(blk[:]); err != nil {
			return err
		}
		extended = blk[offSparseIsExtended] != 0
	}
	return nil
}

// nextIsHeader peeks at the next block without consuming it and reports
// whether it is the end of input, an end-of-archive block, or a block with a
// valid header checksum, meaning no payload precedes it.
func (d *decomposer) nextIsHeader() bool {
	b, err := d.in.Peek(blockSize)
	if err != nil {
		return true
	}
	return isZeroBlock(b) || checksumOK(b)
}

// extract streams size bytes of file content into a new blob, records the
// index entry, and copies the block padding to the recipe.
func (d *decomposer) extract(name string, size int64, hdrPos int64) error {
	rel := blobName(len(d.entries) + 1)
	f, err := os.Create(filepath.Join(d.dir, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("creating blob: %w", err)
	}
	offset := d.outPos
	n, err := io.CopyN(f, d.in, size)
	d.inPos += n
	if err != nil {
		f.Close()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("offset %d: entry %q: truncated content (%d of %d bytes)", hdrPos, name, n, size)
		}
		return fmt.Errorf("writing blob %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing blob %s: %w", rel, err)
	}
	d.entries = append(d.entries, Entry{Name: name, Offset: offset, Size: size, Blob: rel})
	return d.copyRecipe(padding(size), hdrPos)
}

// readMeta reads a small meta-entry payload into memory, writing it and its
// padding to the recipe as well.
func (d *decomposer) readMeta(size int64, hdrPos int64) ([]byte, error) {
	if size > maxMetaSize {
		return nil, fmt.Errorf("offset %d: meta entry of %d bytes exceeds the %d byte limit", hdrPos, size, maxMetaSize)
	}
	payload := make([]byte, size)
	n, err := io.ReadFull(d.in, payload)
	d.inPos += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("offset %d: truncated meta entry (%d of %d bytes)", hdrPos, n, size)
		}
		return nil, fmt.Errorf("offset %d: reading meta entry: %w", hdrPos, err)
	}
	if err := d.writeRecipe(payload); err != nil {
		return nil, err
	}
	return payload, d.copyRecipe(padding(size), hdrPos)
}

// copyPayload copies an entry's payload and padding to the recipe.
func (d *decomposer) copyPayload(size int64, hdrPos int64) error {
	return d.copyRecipe(size+padding(size), hdrPos)
}

// copyRecipe copies n bytes from the archive to the recipe.
func (d *decomposer) copyRecipe(n int64, hdrPos int64) error {
	m, err := io.CopyN(d.recipe, d.in, n)
	d.inPos += m
	d.outPos += m
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("offset %d: truncated entry (%d of %d bytes)", hdrPos, m, n)
		}
		return fmt.Errorf("writing recipe: %w", err)
	}
	return nil
}

func (d *decomposer) writeRecipe(b []byte) error {
	n, err := d.recipe.Write(b)
	d.outPos += int64(n)
	if err != nil {
		return fmt.Errorf("writing recipe: %w", err)
	}
	return nil
}
