package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Store is a content-addressed store for the original, uncompacted tool-result
// payloads. When retrieval is enabled, sieve sends the compacted form upstream
// but keeps the full original here, keyed by a hash of its content, so it can be
// pulled back on demand — by the sieve_fetch tool (via the `sieve mcp` server)
// or the /sieve/fetch HTTP endpoint.
//
// This is the "context rolling" buffer the goal describes: the bulky original
// lives out-of-band, a compact stand-in carrying a ref lives in the context, and
// the model retrieves the original only if it actually needs it — so the tokens
// are spent only on demand instead of on every turn.
//
// Two properties matter:
//
//   - Content-addressed. ref = sha256(original)[:12], so identical content always
//     maps to the same ref. Storing is idempotent, and the ref embedded in a
//     compaction marker is a pure function of the content — keeping the compacted
//     output byte-stable across turns (and so cache-prefix-preserving).
//
//   - Filesystem-backed. The proxy and a separately-launched `sieve mcp` process
//     don't share memory, so the store lives under ~/.sieve/store and they
//     coordinate through it. Files are written 0600.
type Store struct {
	dir  string
	mu   sync.Mutex
	seen map[string]bool // refs already persisted this process (skip re-writes)
}

var refPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// storeDir resolves the store location. Override with SIEVE_STORE_DIR; defaults
// to ~/.sieve/store (mirrors authPath's ~/.sieve layout).
func storeDir() string {
	if p := os.Getenv("SIEVE_STORE_DIR"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".sieve-store"
	}
	return filepath.Join(home, ".sieve", "store")
}

func NewStore() *Store {
	return &Store{dir: storeDir(), seen: map[string]bool{}}
}

// refOf returns the content address of a payload.
func refOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// Put persists the original payload (if not already stored) and returns its ref.
// Idempotent: the same content yields the same ref and is written at most once
// per process.
func (s *Store) Put(original string) (string, error) {
	ref := refOf([]byte(original))

	s.mu.Lock()
	already := s.seen[ref]
	s.mu.Unlock()
	if already {
		return ref, nil
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return ref, err
	}
	path := filepath.Join(s.dir, ref)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Content-addressed, so concurrent writers would write identical bytes.
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			return ref, err
		}
	}

	s.mu.Lock()
	s.seen[ref] = true
	s.mu.Unlock()
	return ref, nil
}

// Get returns the original payload for a ref. The ref is validated against a
// strict hex pattern first, so a caller-supplied ref can never escape the store
// directory.
func (s *Store) Get(ref string) (string, bool) {
	if !refPattern.MatchString(ref) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(s.dir, ref))
	if err != nil {
		return "", false
	}
	return string(b), true
}
