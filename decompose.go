package tarprism

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"lukechampine.com/blake3"
)

// maxMetaSize bounds the payload of PAX and GNU long-name entries, which are
// held in memory while they are parsed.
const maxMetaSize = 256 << 20

const bufSize = 1 << 20

// Decompose reads an uncompressed tar archive from r and writes a prism into
// dir: recipe.bin, recipe.json, and blobs/. dir must not exist or must be
// empty. On error the partially written directory is left in place.
//
// It is DecomposeTo(r, DirSink(dir)).
func Decompose(r io.Reader, dir string) error {
	return DecomposeTo(r, DirSink(dir))
}

// DecomposeTo reads an uncompressed tar archive from r and hands its parts to
// sink: the recipe as a stream, each regular file's content as it is reached,
// and the index last. The recipe writer is closed before Index is called, and
// also when decomposition fails; on failure Index is not called and whatever
// the sink already received is not a valid prism.
func DecomposeTo(r io.Reader, sink Sink) error {
	recipe, err := sink.Recipe()
	if err != nil {
		return err
	}
	hasher := blake3.New(32, nil)
	d := &decomposer{
		in:      bufio.NewReaderSize(io.TeeReader(r, hasher), bufSize),
		recipe:  bufio.NewWriterSize(recipe, bufSize),
		sink:    sink,
		entries: []Entry{},
	}
	if err := d.run(); err != nil {
		recipe.Close()
		return err
	}
	if err := d.recipe.Flush(); err != nil {
		recipe.Close()
		return fmt.Errorf("writing recipe: %w", err)
	}
	if err := recipe.Close(); err != nil {
		return fmt.Errorf("closing recipe: %w", err)
	}
	return sink.Index(&Index{
		Version: FormatVersion,
		BLAKE3:  hex.EncodeToString(hasher.Sum(nil)),
		Entries: d.entries,
	})
}

// decomposer walks the archive block by block, sending regular-file content
// to the sink's blobs and everything else to the recipe.
type decomposer struct {
	in      *bufio.Reader
	recipe  *bufio.Writer
	sink    Sink
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

// extract hands size bytes of file content to the sink as a new blob, records
// the index entry, and copies the block padding to the recipe.
func (d *decomposer) extract(name string, size int64, hdrPos int64) error {
	i := len(d.entries)
	entry := Entry{Name: name, Offset: d.outPos, Size: size, Blob: blobName(i + 1)}
	br := &blobReader{r: d.in, remaining: size}
	err := d.sink.Blob(i, entry, br)
	d.inPos += br.n
	switch {
	case br.err != nil && !errors.Is(br.err, io.EOF):
		return fmt.Errorf("offset %d: entry %q: reading content: %w", hdrPos, name, br.err)
	case br.n < size && br.err != nil:
		return fmt.Errorf("offset %d: entry %q: truncated content (%d of %d bytes)", hdrPos, name, br.n, size)
	case err != nil:
		return fmt.Errorf("offset %d: entry %q: sink: %w", hdrPos, name, err)
	case br.n < size:
		return fmt.Errorf("offset %d: entry %q: sink consumed %d of %d bytes", hdrPos, name, br.n, size)
	}
	d.entries = append(d.entries, entry)
	return d.copyRecipe(padding(size), hdrPos)
}

// blobReader hands a sink exactly the content bytes of one entry. It counts
// what the sink consumed and remembers how the archive reader ended so that
// extract can tell a lazy sink from a truncated archive.
type blobReader struct {
	r         io.Reader
	remaining int64
	n         int64 // bytes handed to the sink
	err       error // first error from r, io.EOF included
}

func (b *blobReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	b.remaining -= int64(n)
	if err != nil && b.err == nil {
		b.err = err
	}
	return n, err
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
