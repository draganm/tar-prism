# tar-prism design

Date: 2026-09-03
Status: approved

## Purpose

`tar-prism` splits an uncompressed tar archive into two parts:

- a **recipe**: every byte of the archive that is not regular-file content
  (headers, PAX and GNU meta entries, block padding, the end-of-archive
  marker, record padding, and any trailing bytes), kept verbatim and in order;
- **blobs**: the content of each regular file, one file per entry.

Together these form a **prism** directory. Composing a prism yields a tar that
is byte-for-byte identical to the original, and the tool verifies this with a
BLAKE3 hash recorded at decompose time.

It ships as a Go library (root package `tarprism`) and a CLI
(`cmd/tar-prism`, built on `github.com/urfave/cli/v2`).

## Non-goals

- Compressed archives (`.tar.gz`, `.tar.zst`, ...). Compressor output is not
  reproducible in general. Callers decompress before and recompress after.
- Content-addressed or path-based blob layouts. Blobs are numbered in tar
  order.
- Pluggable storage backends. The API works on a directory on disk.
- Interpreting archive semantics (extracting files, resolving links, applying
  permissions). The tool only needs to know where file content starts and ends.

## Prism directory format

```
<prism>/
  recipe.bin        non-content bytes of the tar, verbatim, in order
  recipe.json       index (schema below)
  blobs/00000001    content of the 1st regular file in tar order
  blobs/00000002    content of the 2nd regular file
  ...
```

Blob names are 1-based decimal, zero-padded to eight digits (longer if needed).
Every regular file gets a blob, including empty ones, so blob N is always the
Nth regular file in the archive.

### recipe.json

```json
{
  "version": 1,
  "blake3": "<64 hex chars: BLAKE3-256 of the original tar>",
  "entries": [
    {"name": "path/in/tar", "offset": 512, "size": 1234, "blob": "blobs/00000001"}
  ]
}
```

- `version`: format version. This document describes version 1.
- `blake3`: hex BLAKE3-256 digest of the entire original archive.
- `entries`: one per blob, in tar order.
  - `offset`: byte position in `recipe.bin` at which the blob content is
    spliced in when composing. Offsets are strictly increasing.
  - `size`: content length in bytes. Must equal the blob file size.
  - `blob`: path of the blob relative to the prism directory.
  - `name`: entry name for human readers, best effort (see below). Never used
    for reconstruction. May be lossy for non-UTF-8 names.

`recipe.json` is written last, so a prism interrupted mid-decompose has no
index and compose fails with a clear error.

## Decompose

Single streaming pass over 512-byte blocks, bounded memory. Input is read once,
through a BLAKE3 hasher so the digest covers exactly the bytes consumed.

Reading loop, per block:

1. Clean EOF at a block boundary: finish. The archive has no end marker; that
   is preserved.
2. Partial block (fewer than 512 bytes): error, unexpected EOF.
3. All-zero block: this is the end-of-archive marker. Write it and every
   remaining input byte, including partial blocks, to the recipe verbatim.
   Finish. Anything after the first zero block is opaque, so concatenated
   archives with zero blocks in the middle round-trip correctly but their
   later files are not extracted as blobs.
4. Otherwise the block is a header. Verify the checksum: sum all 512 bytes
   with the checksum field treated as eight spaces, and accept if the parsed
   octal checksum matches either the unsigned or the signed (int8) sum. On
   mismatch: error, invalid header.
5. Parse `typeflag` (byte 156) and `size` (bytes 124..136). Size is base-256
   two's complement when the first byte has the high bit set, otherwise octal
   with leading/trailing spaces and NULs trimmed. Negative or unparseable:
   error.
6. If a PAX `size` record is pending from a preceding `x` entry, it replaces
   the header size. The pending PAX and GNU long-name state is cleared once
   applied to a non-meta entry.
7. Dispatch on `typeflag`:

| typeflag | handling |
|---|---|
| `0`, NUL, `7` (regular, contiguous) | header to recipe; `size` bytes to a new blob; padding to recipe; append index entry |
| NUL with name ending in `/` | treated as directory (header-only rule) |
| `S` (old GNU sparse) | header to recipe; while the `isextended` flag is set (byte 482 of the header, byte 504 of each extension block) copy the next 512-byte block to recipe; then `size` bytes to a blob; padding to recipe |
| `x` (PAX extended) | header, payload, padding to recipe; parse records; remember `size` and `path` for the next entry |
| `g` (PAX global) | header, payload, padding to recipe; records ignored (same as Go's archive/tar) |
| `L` (GNU long name) | header, payload, padding to recipe; remember payload as the next entry's name |
| `K` (GNU long link) | header, payload, padding to recipe |
| `1`..`6` (hard link, symlink, char, block, dir, fifo) | header-only rule |
| anything else | header, `size` bytes, padding to recipe |

**Header-only rule.** These entry types have no payload by convention, but
some writers store a nonzero size anyway (old V7 hard links, some directory
entries), while PAX allows real payloads on hard links. Resolution:

- size is zero: no payload.
- a PAX `size` record applied to this entry: obey it, payload of that size
  goes to the recipe.
- otherwise peek at the next 512 bytes without consuming them. If it is EOF,
  an all-zero block, or a block that passes the checksum test, the size is
  bogus: treat as no payload. Otherwise the bytes are a real payload: copy
  `size` bytes plus padding to the recipe.

This is the same heuristic libarchive uses for hard links, applied uniformly.

**Padding** is `(512 - size%512) % 512` bytes and always goes to the recipe
verbatim, even if it is not zeros.

**PAX records** have the form `"%d %s=%s\n"` where the leading decimal is the
total record length including itself and the newline. A record that does not
fit this form, or a `size` value that is not a non-negative decimal integer,
is an error. Only `size` and `path` are used.

**Name derivation** (informational only): GNU long name if pending, else PAX
`path` if pending, else for ustar magic (`ustar\0` + version `00`) with a
nonempty prefix field `prefix + "/" + name`, else `name`. Each field is cut
at its first NUL.

**Output directory**: created if missing. If it exists it must be empty,
otherwise error. `blobs/` is created inside it. Nothing is cleaned up on
error by the library; the CLI removes the directory on failure only if it
created it.

## Compose

Parser-free. Steps:

1. Read and validate `recipe.json`: version must be 1, `blake3` must be 64
   hex characters, offsets must be strictly increasing.
2. Wrap the output writer with a buffered writer and a BLAKE3 hasher.
3. Open `recipe.bin`. For each entry in order: copy recipe bytes up to
   `offset` (error if the recipe is shorter), stat the blob and error if its
   size differs from `size`, then copy exactly `size` bytes from it (error if
   fewer are read).
4. Copy the remainder of `recipe.bin`.
5. Flush, then compare the BLAKE3 digest of everything written with the
   recorded one. Mismatch is an error. The output has already been written by
   then; callers should treat a compose error as "output is not trustworthy".

## Library API

```go
package tarprism

const (
    RecipeFile = "recipe.bin"
    IndexFile  = "recipe.json"
    BlobsDir   = "blobs"
)

// Decompose reads an uncompressed tar from r and writes a prism into dir.
// dir must not exist or must be empty.
func Decompose(r io.Reader, dir string) error

// Compose reads the prism in dir and writes the original tar to w, verifying
// the result against the recorded BLAKE3 digest.
func Compose(dir string, w io.Writer) error

// ReadIndex parses and validates <dir>/recipe.json.
func ReadIndex(dir string) (*Index, error)

type Index struct {
    Version int     `json:"version"`
    BLAKE3  string  `json:"blake3"`
    Entries []Entry `json:"entries"`
}

type Entry struct {
    Name   string `json:"name"`
    Offset int64  `json:"offset"`
    Size   int64  `json:"size"`
    Blob   string `json:"blob"`
}
```

Errors are wrapped with context (which file, which byte offset in the
archive, which entry) using `%w`.

## CLI

```
tar-prism decompose <input.tar|-> <prism-dir>
tar-prism compose   <prism-dir> <output.tar|->
```

- `-` means stdin for input and stdout for output.
- Exactly two positional arguments per command; otherwise a usage error.
- `compose` overwrites an existing output file (tar-like behaviour).
- `decompose` refuses a non-empty existing directory and, on failure, removes
  the directory if it created it.
- Errors print to stderr and exit with status 1.
- The `App` is constructed by a function that takes stdin/stdout streams so
  tests can drive it without touching the process streams.

## Code layout

```
tarprism.go       constants, Index/Entry, ReadIndex
header.go         block parsing: checksum, numeric fields, typeflag, name, PAX records
decompose.go      Decompose
compose.go        Compose
cmd/tar-prism/
  main.go         urfave/cli/v2 app wiring
```

## Dependencies

- `github.com/urfave/cli/v2` (CLI, per user request)
- `lukechampine.com/blake3` (BLAKE3, per user request for a fast hash)
- standard library otherwise. `archive/tar` is used in tests to generate
  fixtures, never in the decompose path.

## Testing

All round-trip tests assert `bytes.Equal(original, composed)` and, where
applicable, that blob contents equal the corresponding file contents and that
the index names match.

- **Header parsing unit tests**: checksum accepted for unsigned and signed
  sums, rejected otherwise; octal and base-256 sizes; PAX record parsing for
  valid, oversized, missing `=`, missing newline, bad `size` value.
- **Generated archives** via `archive/tar` in ustar, PAX, and GNU formats:
  short and long names (>100 chars, needing PAX path or GNU `L`), long link
  targets, directories, symlinks, hard links, empty files, files whose size is
  an exact multiple of 512, a multi-megabyte file, and a variant padded to a
  10240-byte record boundary.
- **Hand-built block fixtures**: old GNU sparse entry with an extension
  block; hard link with nonzero size followed by a valid header (no payload);
  hard link with nonzero size followed by payload bytes; NUL typeflag with
  trailing `/` and nonzero size; PAX `size` overriding the ustar size field;
  unknown typeflag `Z` with payload; trailing garbage after the end marker;
  archive with no end marker; empty (0-byte) archive.
- **Error cases**: truncated header, truncated payload, bad checksum,
  unparseable size, malformed PAX record; compose with missing blob, blob size
  mismatch, tampered blob of equal size (hash mismatch), unsupported version,
  non-increasing offsets, non-empty target directory.
- **System tar archives**, skipped when no binary is found: the test probes
  `tar`, `gtar`, `bsdtar`, and `/usr/bin/tar`, classifies each by its
  `--version` output, and uses one GNU tar and one bsdtar if available (the
  dev shell provides GNU tar via nixpkgs; macOS ships bsdtar at
  `/usr/bin/tar`). bsdtar archives use `--format ustar`, `pax`, `gnutar`; GNU
  tar archives use `--format=ustar`, `posix`, `gnu`, and `-S` on a sparse
  file. The source tree includes long names, a symlink, a hard link, an empty
  file, and a directory.
- **CLI tests**: decompose and compose through the app with file paths and
  with `-` streams, argument count errors, and non-zero exit on failure.
