package ingestion

import "testing"

func TestFingerprintStableAndCaseInsensitive(t *testing.T) {
	stack := "at foo (a.go:1)\nat bar (b.go:2)\nat baz (c.go:3)\nat qux (d.go:4)"

	a := Fingerprint("Null Pointer", stack)
	b := Fingerprint("null pointer", stack)
	if a != b {
		t.Fatalf("expected case-insensitive message to match: %s != %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-char sha256 hex, got %d", len(a))
	}
}

func TestFingerprintUsesFirstThreeStackLines(t *testing.T) {
	// The 4th+ lines must not influence the fingerprint.
	base := "line one\nline two\nline three"
	withExtra := base + "\nline four\nline five"

	if got, want := Fingerprint("msg", withExtra), Fingerprint("msg", base); got != want {
		t.Fatalf("expected only first 3 stack lines to matter: %s != %s", got, want)
	}
}

func TestFingerprintIgnoresBlankLines(t *testing.T) {
	withBlanks := "\n\nline one\n\nline two\n   \nline three"
	compact := "line one\nline two\nline three"

	if got, want := Fingerprint("msg", withBlanks), Fingerprint("msg", compact); got != want {
		t.Fatalf("blank lines should be ignored: %s != %s", got, want)
	}
}

func TestFingerprintDiffersByMessage(t *testing.T) {
	stack := "line one\nline two\nline three"
	if Fingerprint("a", stack) == Fingerprint("b", stack) {
		t.Fatal("different messages should produce different fingerprints")
	}
}
