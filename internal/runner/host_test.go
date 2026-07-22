package runner

import (
	"bytes"
	"testing"
)

func TestNormalizeToolNamePrettierVariant(t *testing.T) {
	if got, want := normalizeToolName("prettier:json"), "prettier"; got != want {
		t.Fatalf("normalizeToolName() = %q, want %q", got, want)
	}
}

func TestNormalizeToolNameUnknownVariant(t *testing.T) {
	if got, want := normalizeToolName("unknown:thing"), "unknown:thing"; got != want {
		t.Fatalf("normalizeToolName() = %q, want %q", got, want)
	}
}

func TestStripGoRunBootstrapStderrRemovesDownloadLines(t *testing.T) {
	stderr := []byte("go: downloading github.com/wasilibs/go-shellcheck v0.11.1\ngo: downloading github.com/tetratelabs/wazero v1.9.0\n")

	if got := stripGoRunBootstrapStderr(stderr); len(got) != 0 {
		t.Fatalf("stripGoRunBootstrapStderr() = %q, want empty", got)
	}
}

func TestStripGoRunBootstrapStderrPreservesRealStderr(t *testing.T) {
	stderr := []byte("go: downloading github.com/wasilibs/go-shellcheck v0.11.1\nreal tool error\n")

	if got, want := stripGoRunBootstrapStderr(stderr), []byte("real tool error\n"); !bytes.Equal(got, want) {
		t.Fatalf("stripGoRunBootstrapStderr() = %q, want %q", got, want)
	}
}
