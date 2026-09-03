package tarprism

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// rawHeader builds a 512-byte header block with a valid checksum. Zero-value
// fields stay NUL, as tar writers leave unused fields.
type rawHeader struct {
	name      string
	typeflag  byte
	size      int64  // encoded as 11 octal digits unless sizeField is set
	sizeField []byte // raw 12-byte size field, overrides size
	magic     string // "ustar\x0000" (POSIX) or "ustar  \x00" (GNU); 8 bytes at offset 257
	linkname  string
	prefix    string
	patch     func(b []byte) // applied before the checksum is computed
}

func (h rawHeader) block() []byte {
	b := make([]byte, blockSize)
	copy(b[0:100], h.name)
	copy(b[100:108], "0000644\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	if h.sizeField != nil {
		copy(b[124:136], h.sizeField)
	} else {
		copy(b[124:136], fmt.Sprintf("%011o\x00", h.size))
	}
	copy(b[136:148], "00000000000\x00")
	b[156] = h.typeflag
	copy(b[157:257], h.linkname)
	copy(b[257:265], h.magic)
	copy(b[345:500], h.prefix)
	if h.patch != nil {
		h.patch(b)
	}
	setChecksum(b, false)
	return b
}

// setChecksum writes the checksum field, using the signed byte sum when
// signed is true.
func setChecksum(b []byte, signed bool) {
	copy(b[148:156], "        ")
	var sum int64
	for _, c := range b {
		if signed {
			sum += int64(int8(c))
		} else {
			sum += int64(c)
		}
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

func TestChecksum(t *testing.T) {
	b := rawHeader{name: "a", typeflag: '0', size: 1}.block()
	if !checksumOK(b) {
		t.Fatal("valid unsigned checksum rejected")
	}
	b[0] = 'b' // corrupt without updating the checksum
	if checksumOK(b) {
		t.Fatal("corrupted header accepted")
	}
	high := rawHeader{name: "\xff\xfe\xfd", typeflag: '0', size: 1}.block()
	setChecksum(high, true)
	if !checksumOK(high) {
		t.Fatal("valid signed checksum rejected")
	}
	if checksumOK(make([]byte, blockSize)) {
		t.Fatal("zero block accepted as header")
	}
}

func TestIsZeroBlock(t *testing.T) {
	if !isZeroBlock(make([]byte, blockSize)) {
		t.Error("zero block not detected")
	}
	b := make([]byte, blockSize)
	b[511] = 1
	if isZeroBlock(b) {
		t.Error("non-zero block reported as zero")
	}
}

func TestParseOctal(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"00000000144\x00", 100, false},
		{"     144 ", 100, false},
		{"\x00\x00\x00", 0, false},
		{"", 0, false},
		{"220\x00\x00garbage", 144, false},
		{"zz", 0, true},
		{"8", 0, true},
		{"7777777777777777777777", 0, true},
	}
	for _, tc := range tests {
		got, err := parseOctal([]byte(tc.in))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseOctal(%q) = %d, %v; want %d, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func base256(v uint64) []byte {
	b := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	b[0] |= 0x80
	return b
}

func TestParseNumeric(t *testing.T) {
	tests := []struct {
		name    string
		in      []byte
		want    int64
		wantErr bool
	}{
		{"octal", []byte("00000000012\x00"), 10, false},
		{"base256 small", base256(3), 3, false},
		{"base256 large", base256(1 << 40), 1 << 40, false},
		{"base256 negative", bytes.Repeat([]byte{0xff}, 12), -1, false},
		{"base256 overflow high bytes", append([]byte{0x80, 0x01}, make([]byte, 10)...), 0, true},
		{"base256 overflow bit 63", append([]byte{0x80, 0, 0, 0, 0x80}, make([]byte, 7)...), 0, true},
	}
	for _, tc := range tests {
		got, err := parseNumeric(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: parseNumeric = %d, %v; want %d, err=%v", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestParseHeader(t *testing.T) {
	ustar := rawHeader{name: "file.txt", prefix: "some/dir", typeflag: '0', size: 7, magic: "ustar\x0000"}.block()
	h, err := parseHeader(ustar)
	if err != nil {
		t.Fatal(err)
	}
	if h.name != "some/dir/file.txt" || h.size != 7 || h.typeflag != '0' || h.isExtended {
		t.Errorf("ustar header = %+v", h)
	}

	gnu := rawHeader{name: "file.txt", prefix: "ignored", typeflag: '0', size: 7, magic: "ustar  \x00"}.block()
	if h, _ := parseHeader(gnu); h.name != "file.txt" {
		t.Errorf("gnu header name = %q, want prefix ignored", h.name)
	}

	sparse := rawHeader{name: "s", typeflag: 'S', size: 512, magic: "ustar  \x00", patch: func(b []byte) { b[offGNUIsExtended] = 1 }}.block()
	if h, _ := parseHeader(sparse); !h.isExtended {
		t.Error("sparse header with isextended flag not detected")
	}

	reg := rawHeader{name: "r", typeflag: '0', size: 1, patch: func(b []byte) { b[offGNUIsExtended] = 1 }}.block()
	if h, _ := parseHeader(reg); h.isExtended {
		t.Error("byte 482 must only mean isextended for typeflag S")
	}

	longPrefix := rawHeader{name: "f", prefix: strings.Repeat("p", 150), typeflag: '0', size: 1, magic: "ustar\x0000"}.block()
	if h, _ := parseHeader(longPrefix); h.name != strings.Repeat("p", 150)+"/f" {
		t.Errorf("long prefix name = %q", h.name)
	}

	if _, err := parseHeader(rawHeader{name: "n", typeflag: '0', sizeField: bytes.Repeat([]byte{0xff}, 12)}.block()); err == nil {
		t.Error("negative size accepted")
	}
	if _, err := parseHeader(rawHeader{name: "b", typeflag: '0', sizeField: []byte("zzzzzzzzzzz\x00")}.block()); err == nil {
		t.Error("unparseable size accepted")
	}
}

func TestParsePAX(t *testing.T) {
	recs, err := parsePAX([]byte("22 path=long/name.txt\n10 size=5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs["path"] != "long/name.txt" || recs["size"] != "5" {
		t.Errorf("records = %v", recs)
	}
	if recs, err := parsePAX(nil); err != nil || len(recs) != 0 {
		t.Errorf("empty payload: %v, %v", recs, err)
	}
	for _, bad := range []string{"5 size", "x size=5\n", "99 size=5\n", "8 size5\n", "0 \n", "size=5\n"} {
		if _, err := parsePAX([]byte(bad)); err == nil {
			t.Errorf("parsePAX(%q) accepted", bad)
		}
	}
}

func TestPadding(t *testing.T) {
	for size, want := range map[int64]int64{0: 0, 1: 511, 512: 0, 513: 511, 1000: 24} {
		if got := padding(size); got != want {
			t.Errorf("padding(%d) = %d, want %d", size, got, want)
		}
	}
}
