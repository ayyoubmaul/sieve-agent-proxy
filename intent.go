package main

import (
	"regexp"
	"strings"
	"sync"
)

// Intent-aware compaction.
//
// The goal asks for compaction "by user intent": keep the keys, log lines, and
// rows the user actually cares about, drop the rest. The tension is that intent
// changes every turn, so shaping output by it would change an old tool result's
// bytes from turn to turn and bust the prompt-cache prefix — the opposite of
// what we want.
//
// The resolution is to PIN. Intent biases the compaction the first time a given
// original is seen (which is the turn it was produced — exactly when current
// intent is most relevant), then the resulting bytes are frozen in a
// process-lifetime cache keyed by the original's content hash. Every later turn
// reuses the pinned bytes verbatim, so the forwarded form stays byte-identical
// and the cache prefix keeps hitting. Intent shapes the first impression; the
// pin keeps it stable forever after.

// compactCtx carries the per-request, server-lifetime state the pure compactors
// can't hold themselves: the retrieval store and the pin cache. A nil ctx (or
// nil field) disables that capability.
type compactCtx struct {
	store *Store
	pin   *pinCache
	// fetchTool is the tool name advertised in the compaction marker — the name
	// the model must call to retrieve an original. Empty falls back to the
	// markerFetchToolDefault.
	fetchTool string
}

// pinCache freezes an original's compacted output so intent-shaped (otherwise
// per-turn-varying) results stay byte-stable across turns.
type pinCache struct {
	mu sync.Mutex
	m  map[string]string
}

func newPinCache() *pinCache { return &pinCache{m: map[string]string{}} }

func (p *pinCache) get(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[key]
	return v, ok
}

func (p *pinCache) put(key, val string) {
	p.mu.Lock()
	p.m[key] = val
	p.mu.Unlock()
}

var reWord = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]{2,}`)

// intentStop are generic words that carry no signal about which fields/rows/lines
// matter. Domain words a user might actually be after (error, test, data, json,
// log, …) are deliberately NOT here.
var intentStop = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "was": true, "this": true,
	"that": true, "with": true, "you": true, "your": true, "our": true, "can": true,
	"could": true, "would": true, "should": true, "please": true, "help": true,
	"need": true, "want": true, "into": true, "from": true, "then": true, "than": true,
	"them": true, "they": true, "what": true, "when": true, "where": true, "which": true,
	"while": true, "will": true, "have": true, "has": true, "had": true, "get": true,
	"see": true, "look": true, "show": true, "give": true, "make": true, "use": true,
	"using": true, "also": true, "but": true, "not": true, "all": true, "any": true,
	"its": true, "let": true, "like": true, "just": true, "now": true, "here": true,
	"there": true, "about": true, "why": true, "how": true, "does": true, "did": true,
}

// extractIntent pulls significant terms from the most recent human user message
// (tool-result-only user messages are skipped, since those are output, not the
// user's ask). Returns lowercased, de-duplicated terms.
func extractIntent(messages []Message) []string {
	text := lastUserText(messages)
	if text == "" {
		return nil
	}
	seen := map[string]bool{}
	var terms []string
	for _, w := range reWord.FindAllString(text, -1) {
		lw := strings.ToLower(w)
		if intentStop[lw] || seen[lw] {
			continue
		}
		seen[lw] = true
		terms = append(terms, lw)
		if len(terms) >= 32 {
			break
		}
	}
	return terms
}

// lastUserText returns the text of the last user message that carries human
// text. Message.Text() concatenates only "text" blocks (and plain-string
// content), so a user message holding only tool_result blocks yields "" and is
// skipped — leaving the user's actual prompt.
func lastUserText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			if t := strings.TrimSpace(messages[i].Text()); t != "" {
				return t
			}
		}
	}
	return ""
}

// matchAnyTerm reports whether s contains any intent term (case-insensitive).
func matchAnyTerm(s string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	ls := strings.ToLower(s)
	for _, t := range terms {
		if strings.Contains(ls, t) {
			return true
		}
	}
	return false
}
