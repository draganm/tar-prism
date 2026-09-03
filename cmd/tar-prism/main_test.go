package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func sampleTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"a.txt", "hello\n"}, {"b.txt", "world\n"}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// run drives the app with the given stdin and arguments, returning stdout.
func run(t *testing.T, stdin []byte, args ...string) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := newApp(bytes.NewReader(stdin), &out).Run(append([]string{"tar-prism"}, args...))
	return out.Bytes(), err
}

func TestFileRoundTrip(t *testing.T) {
	archive := sampleTar(t)
	tmp := t.TempDir()
	in := filepath.Join(tmp, "in.tar")
	if err := os.WriteFile(in, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "prism")
	out := filepath.Join(tmp, "out.tar")
	if _, err := run(t, nil, "decompose", in, dir); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if _, err := run(t, nil, "compose", dir, out); err != nil {
		t.Fatalf("compose: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Fatal("composed archive differs from the original")
	}
}

func TestStdioRoundTrip(t *testing.T) {
	archive := sampleTar(t)
	dir := filepath.Join(t.TempDir(), "prism")
	if _, err := run(t, archive, "decompose", "-", dir); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	got, err := run(t, nil, "compose", dir, "-")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if !bytes.Equal(got, archive) {
		t.Fatal("composed archive differs from the original")
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"decompose"}, {"decompose", "a"}, {"decompose", "a", "b", "c"},
		{"compose"}, {"compose", "a"}, {"compose", "a", "b", "c"},
	} {
		if _, err := run(t, nil, args...); err == nil {
			t.Errorf("%v: expected a usage error", args)
		}
	}
}

func TestDecomposeFailureCleanup(t *testing.T) {
	truncated := sampleTar(t)[:100]
	tmp := t.TempDir()
	in := filepath.Join(tmp, "in.tar")
	if err := os.WriteFile(in, truncated, 0o644); err != nil {
		t.Fatal(err)
	}

	created := filepath.Join(tmp, "new")
	if _, err := run(t, nil, "decompose", in, created); err == nil {
		t.Fatal("expected an error for a truncated archive")
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Errorf("directory created by the failed run was not removed (stat err = %v)", err)
	}

	existing := filepath.Join(tmp, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, nil, "decompose", in, existing); err == nil {
		t.Fatal("expected an error for a truncated archive")
	}
	if _, err := os.Stat(existing); err != nil {
		t.Errorf("pre-existing directory was removed: %v", err)
	}
}

func TestMissingInputs(t *testing.T) {
	tmp := t.TempDir()
	if _, err := run(t, nil, "decompose", filepath.Join(tmp, "nope.tar"), filepath.Join(tmp, "p")); err == nil {
		t.Error("decompose of a missing file succeeded")
	}
	if _, err := run(t, nil, "compose", filepath.Join(tmp, "not-a-prism"), "-"); err == nil {
		t.Error("compose of a missing directory succeeded")
	}
}
