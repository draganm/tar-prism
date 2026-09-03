# tar-prism

Splits an uncompressed tar archive into two parts and puts it back together
byte for byte:

- **recipe** — every byte that is not regular-file content: headers, PAX and
  GNU meta entries, block padding, the end-of-archive marker, trailing
  padding. Kept verbatim, in order.
- **blobs** — the content of each regular file, numbered in archive order.

Together they form a *prysm* directory:

```
prysm/
  recipe.bin        non-content bytes of the archive, verbatim
  recipe.json       splice offsets, sizes, names, and the BLAKE3 digest of the archive
  blobs/00000001    content of the 1st regular file
  blobs/00000002    content of the 2nd regular file
  ...
```

Composing a prysm reproduces the original archive exactly and verifies the
result against the recorded BLAKE3 digest.

## CLI

```
tar-prism decompose <input.tar|-> <prysm-dir>
tar-prism compose   <prysm-dir> <output.tar|->
```

`-` reads the archive from stdin or writes it to stdout. `decompose` refuses a
non-empty target directory. `compose` overwrites an existing output file. If
`compose` fails, the output file may be partial or unverified; trust it only
when the command exits with status 0. Only uncompressed archives are
supported; decompress `.tar.gz` and friends first.

## Library

```go
import tarprism "github.com/draganm/tar-prism"

err := tarprism.Decompose(reader, "prysm")   // tar in, prysm directory out
err  = tarprism.Compose("prysm", writer)     // prysm directory in, identical tar out
idx, err := tarprism.ReadIndex("prysm")      // inspect recipe.json
```

## Development

```
nix develop            # Go and GNU tar from nixpkgs (direnv: `direnv allow`)
go test ./...
```

The tests round-trip archives written by Go's `archive/tar`, by GNU tar and
bsdtar when available, and hand-built block sequences covering GNU sparse
entries, PAX size overrides, bogus hard-link sizes, and truncated input.
