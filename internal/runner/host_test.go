package runner

import "testing"

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
