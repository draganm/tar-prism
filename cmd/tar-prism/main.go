// Command tar-prism splits a tar archive into a recipe and content blobs, and
// reassembles the byte-identical archive from them.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	tarprism "github.com/draganm/tar-prism"
	"github.com/urfave/cli/v2"
)

func main() {
	if err := newApp(os.Stdin, os.Stdout).Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "tar-prism:", err)
		os.Exit(1)
	}
}

func newApp(stdin io.Reader, stdout io.Writer) *cli.App {
	return &cli.App{
		Name:   "tar-prism",
		Usage:  "split a tar into a recipe and blobs, and put it back together byte for byte",
		Writer: stdout,
		Commands: []*cli.Command{
			{
				Name:      "decompose",
				Usage:     "split an uncompressed tar archive into a prysm directory",
				ArgsUsage: "<input.tar|-> <prysm-dir>",
				Action: func(c *cli.Context) error {
					if c.NArg() != 2 {
						return errors.New("usage: tar-prism decompose <input.tar|-> <prysm-dir>")
					}
					return decompose(stdin, c.Args().Get(0), c.Args().Get(1))
				},
			},
			{
				Name:      "compose",
				Usage:     "rebuild the original tar archive from a prysm directory",
				ArgsUsage: "<prysm-dir> <output.tar|->",
				Action: func(c *cli.Context) error {
					if c.NArg() != 2 {
						return errors.New("usage: tar-prism compose <prysm-dir> <output.tar|->")
					}
					return compose(stdout, c.Args().Get(0), c.Args().Get(1))
				},
			},
		},
	}
}

// decompose reads the archive from input ("-" for stdin) into dir. If dir did
// not exist before and decomposition fails, dir is removed again.
func decompose(stdin io.Reader, input, dir string) error {
	in := stdin
	if input != "-" {
		f, err := os.Open(input)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	_, statErr := os.Stat(dir)
	created := errors.Is(statErr, os.ErrNotExist)
	if err := tarprism.Decompose(in, dir); err != nil {
		if created {
			os.RemoveAll(dir)
		}
		return err
	}
	return nil
}

// compose writes the archive in dir to output ("-" for stdout), overwriting
// an existing file.
func compose(stdout io.Writer, dir, output string) error {
	if output == "-" {
		return tarprism.Compose(dir, stdout)
	}
	f, err := os.Create(output)
	if err != nil {
		return err
	}
	if err := tarprism.Compose(dir, f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
