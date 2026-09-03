package tarprism

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTripFixtures(t *testing.T) {
	gnu := "ustar  \x00"
	posix := "ustar\x0000"
	regB := rawHeader{name: "b", typeflag: '0', size: 3, magic: posix}.block()
	bodyB := payload([]byte("abc"))
	abc := [][]byte{[]byte("abc")}

	sparseData := bytes.Repeat([]byte("S"), 1024)
	extension := make([]byte, blockSize) // its own isextended flag (byte 504) is zero: last one

	base256Three := append([]byte{0x80}, make([]byte, 11)...)
	base256Three[11] = 3

	fixtures := []struct {
		name     string
		archive  []byte
		names    []string
		contents [][]byte
		offsets  []int64
	}{
		{
			name: "old gnu sparse with extension block",
			archive: concat(
				rawHeader{name: "s", typeflag: 'S', size: 1024, magic: gnu, patch: func(b []byte) { b[offGNUIsExtended] = 1 }}.block(),
				extension, payload(sparseData), endMarker),
			names: []string{"s"}, contents: [][]byte{sparseData}, offsets: []int64{1024},
		},
		{
			name: "hard link with bogus size before a header",
			archive: concat(
				rawHeader{name: "h", typeflag: '1', linkname: "b", size: 100, magic: posix}.block(),
				regB, bodyB, endMarker),
			names: []string{"b"}, contents: abc, offsets: []int64{1024},
		},
		{
			name: "hard link with real payload",
			archive: concat(
				rawHeader{name: "h", typeflag: '1', linkname: "b", size: 5, magic: posix}.block(),
				payload([]byte("hello")), regB, bodyB, endMarker),
			names: []string{"b"}, contents: abc, offsets: []int64{1536},
		},
		{
			name: "directory with bogus size at end of archive",
			archive: concat(
				rawHeader{name: "d/", typeflag: '5', size: 4096, magic: posix}.block(), endMarker),
		},
		{
			name: "pre-posix directory with bogus size",
			archive: concat(
				rawHeader{name: "olddir/", typeflag: 0, size: 100}.block(),
				regB, bodyB, endMarker),
			names: []string{"b"}, contents: abc, offsets: []int64{1024},
		},
		{
			name: "pre-posix regular file",
			archive: concat(
				rawHeader{name: "old", typeflag: 0, size: 3}.block(), bodyB, endMarker),
			names: []string{"old"}, contents: abc, offsets: []int64{512},
		},
		{
			name: "pax size and path override",
			archive: concat(
				rawHeader{name: "PaxHeader/f", typeflag: 'x', size: 32, magic: posix}.block(),
				payload([]byte("22 path=long/name.txt\n10 size=5\n")),
				rawHeader{name: "f", typeflag: '0', sizeField: []byte("00000000000\x00"), magic: posix}.block(),
				payload([]byte("hello")), endMarker),
			names: []string{"long/name.txt"}, contents: [][]byte{[]byte("hello")}, offsets: []int64{1536},
		},
		{
			name: "pax size on a hard link is obeyed",
			archive: concat(
				rawHeader{name: "PaxHeader/h", typeflag: 'x', size: 10, magic: posix}.block(),
				payload([]byte("10 size=5\n")),
				rawHeader{name: "h", typeflag: '1', linkname: "b", size: 5, magic: posix}.block(),
				payload([]byte("hello")), regB, bodyB, endMarker),
			names: []string{"b"}, contents: abc, offsets: []int64{2560},
		},
		{
			name: "pax global header is ignored",
			archive: concat(
				rawHeader{name: "GlobalHead", typeflag: 'g', size: 10, magic: posix}.block(),
				payload([]byte("10 size=9\n")),
				regB, bodyB, endMarker),
			names: []string{"b"}, contents: abc, offsets: []int64{1536},
		},
		{
			name: "gnu long name and long link",
			archive: concat(
				rawHeader{name: "././@LongLink", typeflag: 'K', size: 7, magic: gnu}.block(),
				payload([]byte("target\x00")),
				rawHeader{name: "././@LongLink", typeflag: 'L', size: 12, magic: gnu}.block(),
				payload([]byte("a/long/name\x00")),
				rawHeader{name: "a/long/name", typeflag: '0', size: 3, magic: gnu}.block(),
				bodyB, endMarker),
			names: []string{"a/long/name"}, contents: abc, offsets: []int64{2560},
		},
		{
			name: "base-256 size",
			archive: concat(
				rawHeader{name: "big", typeflag: '0', sizeField: base256Three, magic: gnu}.block(), bodyB, endMarker),
			names: []string{"big"}, contents: abc, offsets: []int64{512},
		},
		{
			name: "unknown typeflag with payload",
			archive: concat(
				rawHeader{name: "weird", typeflag: 'Z', size: 3, magic: posix}.block(), payload([]byte("zzz")),
				regB, bodyB, endMarker),
			names: []string{"b"}, contents: abc, offsets: []int64{1536},
		},
		{
			name: "ustar prefix sets byte 482 on a regular file",
			archive: concat(
				rawHeader{name: "f", prefix: strings.Repeat("p", 150), typeflag: '0', size: 3, magic: posix}.block(),
				bodyB, endMarker),
			names: []string{strings.Repeat("p", 150) + "/f"}, contents: abc, offsets: []int64{512},
		},
		{
			name:    "non-zero padding bytes are preserved",
			archive: concat(regB, append([]byte("abc"), bytes.Repeat([]byte{0xAA}, 509)...), endMarker),
			names:   []string{"b"}, contents: abc, offsets: []int64{512},
		},
		{
			name:    "trailing garbage after end marker",
			archive: concat(regB, bodyB, endMarker, []byte("this is not a tar block")),
			names:   []string{"b"}, contents: abc, offsets: []int64{512},
		},
		{
			name:    "no end marker",
			archive: concat(regB, bodyB),
			names:   []string{"b"}, contents: abc, offsets: []int64{512},
		},
		{
			name:    "zero block in the middle hides later files",
			archive: concat(regB, bodyB, make([]byte, blockSize), regB, bodyB, endMarker),
			names:   []string{"b"}, contents: abc, offsets: []int64{512},
		},
		{
			name:    "empty archive",
			archive: nil,
		},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			composed, dir, idx := roundTrip(t, fx.archive)
			assertIdentical(t, fx.archive, composed)
			assertBlobs(t, dir, idx, fx.names, fx.contents)
			if len(fx.offsets) != len(idx.Entries) {
				t.Fatalf("fixture lists %d offsets for %d entries", len(fx.offsets), len(idx.Entries))
			}
			for i, e := range idx.Entries {
				if e.Offset != fx.offsets[i] {
					t.Errorf("entry %d: offset %d, want %d", i, e.Offset, fx.offsets[i])
				}
			}
		})
	}
}

func TestDecomposeErrors(t *testing.T) {
	posix := "ustar\x0000"
	regB := rawHeader{name: "b", typeflag: '0', size: 3, magic: posix}.block()
	badChecksum := append([]byte{}, regB...)
	badChecksum[0] = 'c'

	cases := []struct {
		name    string
		archive []byte
		wantErr string
	}{
		{"truncated header", regB[:100], "truncated header"},
		{"truncated content", concat(regB, []byte("ab")), "truncated content"},
		{"truncated padding", concat(regB, []byte("abc")), "truncated entry"},
		{"bad checksum", concat(badChecksum, payload([]byte("abc")), endMarker), "invalid header checksum"},
		{"bad size", rawHeader{name: "b", typeflag: '0', sizeField: []byte("zzzzzzzzzzz\x00")}.block(), "size field"},
		{"malformed pax", concat(rawHeader{name: "x", typeflag: 'x', size: 6}.block(), payload([]byte("5 size")), regB, payload([]byte("abc")), endMarker), "pax record"},
		{"bad pax size", concat(rawHeader{name: "x", typeflag: 'x', size: 11}.block(), payload([]byte("11 size=-1\n")), regB, payload([]byte("abc")), endMarker), "invalid pax size"},
		{"truncated meta entry", concat(rawHeader{name: "x", typeflag: 'x', size: 10}.block(), []byte("10 si")), "truncated meta entry"},
		{"oversized meta entry", rawHeader{name: "x", typeflag: 'x', size: maxMetaSize + 1}.block(), "exceeds"},
		{"truncated sparse extension", rawHeader{name: "s", typeflag: 'S', size: 0, magic: "ustar  \x00", patch: func(b []byte) { b[offGNUIsExtended] = 1 }}.block(), "sparse extension"},
		{"size field would overflow size+padding", concat(rawHeader{name: "weird", typeflag: 'Z', sizeField: []byte{0x80, 0x00, 0x00, 0x00, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}.block(), endMarker), "size field"},
		{"pax size would overflow size+padding", concat(rawHeader{name: "x", typeflag: 'x', size: 28}.block(), payload([]byte("28 size=9223372036854775807\n")), regB, payload([]byte("abc")), endMarker), "invalid pax size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "prysm")
			err := Decompose(bytes.NewReader(tc.archive), dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
