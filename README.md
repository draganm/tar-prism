# tar-prism

Splits an uncompressed tar archive into two parts and puts it back together
byte for byte:

- **recipe** — every byte that is not regular-file content: headers, PAX and
  GNU meta entries, block padding, the end-of-archive marker, trailing
  padding. Kept verbatim, in order.
- **blobs** — the content of each regular file, numbered in archive order.

Together they form a *prism* directory:

```
prism/
  recipe.bin        non-content bytes of the archive, verbatim
  recipe.json       splice offsets, sizes, names, and the BLAKE3 digest of the archive
  blobs/00000001    content of the 1st regular file
  blobs/00000002    content of the 2nd regular file
  ...
```

Composing a prism reproduces the original archive exactly and verifies the
result against the recorded BLAKE3 digest.

## CLI

```
tar-prism decompose <input.tar|-> <prism-dir>
tar-prism compose   <prism-dir> <output.tar|->
```

`-` reads the archive from stdin or writes it to stdout. `decompose` refuses a
non-empty target directory. `compose` overwrites an existing output file. If
`compose` fails, the output file may be partial or unverified; trust it only
when the command exits with status 0. Only uncompressed archives are
supported; decompress `.tar.gz` and friends first.

## Library

```go
import tarprism "github.com/draganm/tar-prism"

err := tarprism.Decompose(reader, "prism")   // tar in, prism directory out
err  = tarprism.Compose("prism", writer)     // prism directory in, identical tar out
idx, err := tarprism.ReadIndex("prism")      // inspect recipe.json
```

### Sinks and sources

The directory functions are adapters over a streaming API, so the parts of a
prism can go anywhere: a content-addressed store, an object store, memory.

```go
// Sink receives the parts of a decomposed archive: Recipe once, then Blob
// once per regular file in archive order, then Index once at the end.
type Sink interface {
    Recipe() (io.WriteCloser, error)
    Blob(index int, entry Entry, r io.Reader) error // must consume exactly entry.Size bytes
    Index(idx *Index) error
}

// Source serves the parts of a prism to ComposeFrom.
type Source interface {
    Index() (*Index, error)
    Recipe() (io.ReadCloser, error)
    Blob(index int, entry Entry) (io.ReadCloser, error) // must yield exactly entry.Size bytes
}

err := tarprism.DecomposeTo(reader, sink)    // tar in, parts to the sink
err  = tarprism.ComposeFrom(source, writer)  // parts from the source, identical tar out

sink := tarprism.DirSink("prism")            // the adapters behind Decompose and Compose
src  := tarprism.DirSource("prism")

data, err := tarprism.EncodeIndex(idx)       // recipe.json bytes, as Decompose writes them
idx, err   = tarprism.DecodeIndex(data)      // parse and validate recipe.json bytes
```

`index` is 0-based and matches `Index.Entries[index]`; `entry.Blob` carries
the `blobs/%08d` name a prism directory would use, so directory and other
sinks agree on naming. `DecomposeTo` closes the recipe writer after the last
recipe byte, before calling `Index`, and also when decomposition fails.
`ComposeFrom` closes every reader it receives and fails if a blob reader ends
before `entry.Size` bytes or has more.

## Development

```
nix develop            # Go and GNU tar from nixpkgs (direnv: `direnv allow`)
go test ./...
```

The tests round-trip archives written by Go's `archive/tar`, by GNU tar and
bsdtar when available, and hand-built block sequences covering GNU sparse
entries, PAX size overrides, bogus hard-link sizes, and truncated input, each
through the directory adapters and through an in-memory sink and source.

## License

tar-prism is licensed under the GNU Affero General Public License, version 3
or later; see `LICENSE`.
