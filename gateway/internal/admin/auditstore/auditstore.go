// Package auditstore adds a durable, queryable trail alongside admin's
// existing structured slog audit lines -- see internal/admin's own
// auditLogger doc comment. That existing mechanism is genuinely
// write-only: a plain slog.Info line has no persistence layer of its
// own and no read/query path beyond whatever the operator's chosen log
// sink happens to support, confirmed by this session's own enterprise-
// procurement-buyer-criteria-2026-09-22.md research finding. This
// package is purely additive: the existing slog line is unchanged and
// keeps flowing to whatever sink it always has, and this ALSO appends
// one JSONL record per event to a dedicated, operator-configured file,
// specifically so it can be queried later without depending on the
// operator's own log-aggregation stack.
package auditstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is one durable audit record. Fields flattens admin's own
// variadic slog args (alternating key, value, key, value...) into a
// plain map of string values -- admin's own call sites pass a
// heterogeneous mix of field names (name/virtual_key_id/id depending on
// which resource the action concerns; see this package's own doc
// comment on why Query filters by arbitrary field name/value rather
// than a single hardcoded "key_id" concept that doesn't uniformly exist
// across them) and mostly-scalar values (string/int/bool) that all
// stringify losslessly for this audit trail's own purpose -- provenance
// lookup, not numeric analysis.
type Entry struct {
	Time   time.Time         `json:"time"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Store appends Entry records to a single JSONL file, one per line, and
// answers Query calls against that same file.
type Store struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// Open opens (creating if absent) path for appending. The file is never
// truncated -- every Append call adds a new line, and a restart resumes
// appending to the same durable history rather than starting fresh.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("auditstore: opening %q: %w", path, err)
	}
	return &Store{f: f, path: path}, nil
}

// Append writes one Entry, built from msg and args (the identical
// alternating key/value pairs admin's own auditLogger.Info already
// receives -- this function has no opinion on what they mean, only on
// storing them). An args slice with an odd length, or a non-string key,
// is a caller bug elsewhere in this codebase (every real call site in
// internal/admin uses literal string keys) -- rather than panicking or
// silently dropping the whole entry, the offending pair is recorded
// under a synthesized key so the rest of the entry is never lost.
func (s *Store) Append(msg string, args ...any) error {
	fields := make(map[string]string, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			key = fmt.Sprintf("arg%d", i)
		}
		if i+1 < len(args) {
			fields[key] = fmt.Sprintf("%v", args[i+1])
		} else {
			fields[key] = ""
		}
	}

	line, err := json.Marshal(Entry{Time: time.Now(), Msg: msg, Fields: fields})
	if err != nil {
		return fmt.Errorf("auditstore: marshaling entry: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("auditstore: writing entry: %w", err)
	}
	return nil
}

// Close closes the underlying file.
func (s *Store) Close() error {
	return s.f.Close()
}

// Query is Store's own convenience wrapper over the package-level Query
// function, reading from the same path this Store appends to -- see
// that function's own doc comment for the full contract.
func (s *Store) Query(filter Filter) ([]Entry, error) {
	return Query(s.path, filter)
}

// Filter selects which Entry records Query returns. A zero-value field
// means "no constraint on this dimension" -- a zero-value Filter matches
// every entry in the file.
type Filter struct {
	// Msg, when non-empty, requires an exact match against Entry.Msg
	// (admin's own action-type strings, e.g. "admin_virtual_key_upserted").
	Msg string
	// Since/Until, when non-zero, bound Entry.Time inclusively at the
	// lower end and exclusively at the upper end -- the same convention
	// a half-open time range interval already uses elsewhere in Go's own
	// standard library (e.g. time.Time.Before/After combinations).
	Since, Until time.Time
	// FieldKey/FieldValue, when FieldKey is non-empty, require
	// Entry.Fields[FieldKey] == FieldValue exactly -- deliberately a
	// single key/value pair, not an arbitrary map, since every real
	// query this surface exists for ("show me everything about this one
	// virtual key/deployment/prompt") is naturally a single-dimension
	// lookup; a caller needing multiple field constraints can filter the
	// returned slice further in Go.
	FieldKey, FieldValue string
}

// matches reports whether e satisfies f.
func (f Filter) matches(e Entry) bool {
	if f.Msg != "" && e.Msg != f.Msg {
		return false
	}
	if !f.Since.IsZero() && e.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !e.Time.Before(f.Until) {
		return false
	}
	if f.FieldKey != "" && e.Fields[f.FieldKey] != f.FieldValue {
		return false
	}
	return true
}

// Query reads every line of path (a file Open/Append already wrote to,
// or one that doesn't exist yet) and returns every Entry matching
// filter, oldest first (the file's own on-disk append order). A
// not-yet-created file (no audit event has ever been appended) returns
// an empty slice, not an error -- the same "nothing recorded yet" case
// as an empty result set, never a caller-visible failure. A single
// malformed line (should never happen against this package's own
// Append, but defends against a hand-edited or truncated-mid-write
// file) is skipped, not fatal to the whole query.
func Query(path string, filter Filter) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("auditstore: opening %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var results []Entry
	scanner := bufio.NewScanner(f)
	// Default bufio.Scanner token limit (64KiB) is enough for any real
	// audit line this package produces (a handful of short field
	// values) -- not raised, since an unusually large line here would
	// itself be a signal worth investigating, not silently accommodating.
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if filter.matches(e) {
			results = append(results, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return results, fmt.Errorf("auditstore: reading %q: %w", path, err)
	}
	return results, nil
}
