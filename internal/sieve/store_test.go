package sieve

import (
	"regexp"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return &Store{dir: t.TempDir(), seen: map[string]bool{}}
}

func TestStoreRoundTrip(t *testing.T) {
	st := newTestStore(t)
	original := strings.Repeat("the original payload\n", 50)

	ref, err := st.Put(original)
	if err != nil {
		t.Fatal(err)
	}
	if !refPattern.MatchString(ref) {
		t.Fatalf("ref %q is not a 12-char hex string", ref)
	}

	// Same content → same ref (content-addressed, idempotent).
	ref2, _ := st.Put(original)
	if ref2 != ref {
		t.Errorf("ref not stable: %q vs %q", ref, ref2)
	}

	got, ok := st.Get(ref)
	if !ok || got != original {
		t.Errorf("round-trip failed: ok=%v len=%d want %d", ok, len(got), len(original))
	}
}

func TestStoreRejectsBadRefs(t *testing.T) {
	st := newTestStore(t)
	for _, bad := range []string{"", "../etc/passwd", "abc", "ZZZZZZZZZZZZ", "0123456789abX", "../../secret"} {
		if _, ok := st.Get(bad); ok {
			t.Errorf("bad ref %q was accepted", bad)
		}
	}
}

func TestCompactToolTextWithRetrievalMarker(t *testing.T) {
	st := newTestStore(t)
	opt := defaultCompactOpts()
	original := strings.Repeat("2024-01-01T10:00:00Z WARN repeated diagnostic line\n", 300)

	out, changed := compactToolText(original, opt, &compactCtx{store: st})
	if !changed {
		t.Fatal("expected compaction")
	}
	if !strings.Contains(out, "sieve_fetch") {
		t.Fatalf("compacted output lacks a sieve_fetch marker:\n%s", out)
	}

	m := regexp.MustCompile(`sieve_fetch\("([0-9a-f]{12})"\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no ref in marker: %s", out)
	}
	got, ok := st.Get(m[1])
	if !ok || got != original {
		t.Errorf("ref %q does not resolve to the original (ok=%v)", m[1], ok)
	}
}

// The marker must use the context's configured fetch tool name, so it always
// names the tool the model can actually call (host-prefixed name).
func TestCompactMarkerUsesConfiguredFetchToolName(t *testing.T) {
	st := newTestStore(t)
	opt := defaultCompactOpts()
	original := strings.Repeat("2024-01-01T10:00:00Z WARN repeated diagnostic line\n", 300)

	out, changed := compactToolText(original, opt, &compactCtx{store: st, fetchTool: "sieve_sieve_fetch"})
	if !changed {
		t.Fatal("expected compaction")
	}
	if !strings.Contains(out, `call sieve_sieve_fetch("`) {
		t.Fatalf("marker should name the configured tool sieve_sieve_fetch:\n%s", out)
	}
	if strings.Contains(out, `call sieve_fetch("`) {
		t.Errorf("marker must not use the bare default when a name is configured:\n%s", out)
	}
}
