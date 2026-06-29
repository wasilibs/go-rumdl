package rumdl

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wasilibs/go-rumdl/internal/runner"
)

//go:embed testdata/in
var inFiles embed.FS

//go:embed testdata/exp
var expFiles embed.FS

//go:embed testdata/rumdl.toml
var rumdlConfig []byte

func TestLint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".rumdl.toml"), rumdlConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	// Tests shellcheck, which is lint only
	if err := os.WriteFile(filepath.Join(dir, "shell.md"), []byte("# Shell\n\n```bash\nif [ $x = 1 ]; then\n  echo hi\nfi\n```\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdin := bytes.Buffer{}
	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}

	ret := runner.Run("rumdl", []string{"check", "shell.md"}, &stdin, &stdout, &stderr, dir)
	out := stdout.String() + stderr.String()
	if ret == 0 {
		t.Fatalf("expected non-zero exit (lint issues), got 0\noutput:\n%s", out)
	}
	if !strings.Contains(out, "[shellcheck]") || !strings.Contains(out, "referenced but not assigned") {
		t.Fatalf("expected shellcheck diagnostics in output:\n%s", out)
	}
}

// TestFormat formats and compares with golden data.
// Run with UPDATE_GOLDEN=1 to regenerate testdata/exp.
func TestFormat(t *testing.T) {
	inFS, err := fs.Sub(inFiles, "testdata/in")
	if err != nil {
		t.Fatal(err)
	}
	expFS, err := fs.Sub(expFiles, "testdata/exp")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".rumdl.toml"), rumdlConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.WalkDir(inFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("rumdl: reading testdata: %w", err)
		}
		if d.IsDir() {
			return nil
		}
		c, _ := fs.ReadFile(inFS, path)
		return os.WriteFile(filepath.Join(dir, path), c, 0o644)
	}); err != nil {
		t.Fatal(err)
	}

	stdin := bytes.Buffer{}
	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}

	ret := runner.Run("rumdl", []string{"fmt", "."}, &stdin, &stdout, &stderr, dir)
	if want := 0; ret != want {
		t.Fatalf("unexpected return code: have %d, want %d\nstdout:\n%s\nstderr:\n%s", ret, want, stdout.String(), stderr.String())
	}

	update := os.Getenv("UPDATE_GOLDEN") == "1"
	err = fs.WalkDir(inFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return fmt.Errorf("rumdl: reading formatted file: %w", err)
		}
		if update {
			if err := os.WriteFile(filepath.Join("testdata/exp", path), got, 0o644); err != nil {
				return fmt.Errorf("rumdl: writing golden: %w", err)
			}
			return nil
		}
		want, err := fs.ReadFile(expFS, path)
		if err != nil {
			return fmt.Errorf("missing golden for %s: %w", path, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if update {
		t.Log("updated testdata/exp")
	}
}
