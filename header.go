package tarprism

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
)

const blockSize = 512

// Field offsets and lengths within a header block (POSIX ustar layout, with
// the GNU sparse extensions that share the same first 345 bytes).
const (
	offName    = 0
	lenName    = 100
	offSize    = 124
	lenSize    = 12
	offChksum  = 148
	lenChksum  = 8
	offType    = 156
	offMagic   = 257
	lenMagic   = 6
	offVersion = 263
	lenVersion = 2
	offPrefix  = 345
	lenPrefix  = 155

	// In an old GNU sparse header ('S'), a nonzero byte here means one or
	// more 512-byte sparse extension blocks follow the header.
	offGNUIsExtended = 482
	// In a sparse extension block, a nonzero byte here means another
	// extension block follows.
	offSparseIsExtended = 504
)

// Typeflags with special handling.
const (
	typeRegA        = 0   // regular file, pre-POSIX; a trailing '/' marks a directory
	typeReg         = '0' // regular file
	typeLink        = '1' // hard link
	typeSymlink     = '2'
	typeChar        = '3'
	typeBlock       = '4'
	typeDir         = '5'
	typeFifo        = '6'
	typeCont        = '7' // contiguous file, treated as regular
	typeXHeader     = 'x' // PAX extended header for the next entry
	typeXGlobal     = 'g' // PAX global header
	typeGNULongName = 'L' // GNU: payload is the next entry's name
	typeGNULongLink = 'K' // GNU: payload is the next entry's link target
	typeGNUSparse   = 'S' // old GNU sparse file
)

var zeroBlock [blockSize]byte

func isZeroBlock(b []byte) bool {
	return bytes.Equal(b, zeroBlock[:])
}

// checksumOK reports whether the header's checksum field matches the sum of
// its bytes with the checksum field itself counted as eight spaces. Both the
// unsigned sum (POSIX) and the signed sum (some historical writers) are
// accepted, as in GNU tar and Go's archive/tar.
func checksumOK(b []byte) bool {
	want, err := parseOctal(b[offChksum : offChksum+lenChksum])
	if err != nil {
		return false
	}
	var unsigned, signed int64
	for i, c := range b {
		if i >= offChksum && i < offChksum+lenChksum {
			c = ' '
		}
		unsigned += int64(c)
		signed += int64(int8(c))
	}
	return want == unsigned || want == signed
}

// header holds the fields of a header block that decomposition depends on.
type header struct {
	typeflag byte
	size     int64
	// name is the ustar name, joined with the prefix field when the block
	// carries the POSIX ustar magic. Informational only.
	name string
	// isExtended is set for old GNU sparse headers that are followed by
	// sparse extension blocks.
	isExtended bool
}

// parseHeader decodes a header block whose checksum has been verified.
func parseHeader(b []byte) (header, error) {
	size, err := parseNumeric(b[offSize : offSize+lenSize])
	if err != nil {
		return header{}, fmt.Errorf("size field: %w", err)
	}
	if size < 0 {
		return header{}, fmt.Errorf("size field: negative size %d", size)
	}
	if size > math.MaxInt64-blockSize {
		return header{}, fmt.Errorf("size field: size %d too large", size)
	}
	h := header{
		typeflag: b[offType],
		size:     size,
		name:     cstring(b[offName : offName+lenName]),
	}
	if h.typeflag == typeGNUSparse && b[offGNUIsExtended] != 0 {
		h.isExtended = true
	}
	isUstar := string(b[offMagic:offMagic+lenMagic]) == "ustar\x00" &&
		string(b[offVersion:offVersion+lenVersion]) == "00"
	if prefix := cstring(b[offPrefix : offPrefix+lenPrefix]); isUstar && prefix != "" {
		h.name = prefix + "/" + h.name
	}
	return h, nil
}

// cstring returns b up to its first NUL as a string.
func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// parseOctal parses an octal field padded with spaces or NULs. An empty field
// is zero.
func parseOctal(b []byte) (int64, error) {
	b = bytes.Trim(b, " \x00")
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	if len(b) == 0 {
		return 0, nil
	}
	n, err := strconv.ParseUint(string(b), 8, 64)
	if err != nil || n > math.MaxInt64 {
		return 0, fmt.Errorf("invalid octal %q", b)
	}
	return int64(n), nil
}

// parseNumeric parses a numeric field that is either octal or, when the high
// bit of the first byte is set, a big-endian two's-complement base-256 number
// (GNU extension for values that do not fit in octal).
func parseNumeric(b []byte) (int64, error) {
	if len(b) == 0 || b[0]&0x80 == 0 {
		return parseOctal(b)
	}
	var inv byte // 0xff when negative, so that -a-1 == ^a
	if b[0]&0x40 != 0 {
		inv = 0xff
	}
	var x uint64
	for i, c := range b {
		c ^= inv
		if i == 0 {
			c &= 0x7f
		}
		if x>>56 != 0 {
			return 0, errors.New("base-256 value overflows int64")
		}
		x = x<<8 | uint64(c)
	}
	if x>>63 != 0 {
		return 0, errors.New("base-256 value overflows int64")
	}
	if inv == 0xff {
		return ^int64(x), nil
	}
	return int64(x), nil
}

// parsePAX splits a PAX extended header payload into its records. Each record
// is "<length> <key>=<value>\n" where length counts the whole record,
// including the length digits and the newline.
func parsePAX(payload []byte) (map[string]string, error) {
	records := map[string]string{}
	for rest := payload; len(rest) > 0; {
		sp := bytes.IndexByte(rest, ' ')
		if sp <= 0 {
			return nil, errors.New("pax record: missing length")
		}
		n, err := strconv.ParseInt(string(rest[:sp]), 10, 64)
		if err != nil || n < int64(sp)+2 || n > int64(len(rest)) {
			return nil, fmt.Errorf("pax record: bad length %q", rest[:sp])
		}
		rec := rest[sp+1 : n]
		rest = rest[n:]
		if rec[len(rec)-1] != '\n' {
			return nil, errors.New("pax record: missing newline")
		}
		eq := bytes.IndexByte(rec, '=')
		if eq < 0 {
			return nil, errors.New("pax record: missing '='")
		}
		records[string(rec[:eq])] = string(rec[eq+1 : len(rec)-1])
	}
	return records, nil
}

// padding returns the number of bytes that follow a payload of the given size
// to reach the next block boundary.
func padding(size int64) int64 {
	return (blockSize - size%blockSize) % blockSize
}
