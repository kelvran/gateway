// Package auditstore adds a durable, queryable trail alongside admin's
// existing structured slog audit lines -- see internal/admin's own
// auditLogger doc comment. That existing mechanism is genuinely
// write-only: a plain slog.Info line has no persistence layer of its
// own and no read/query path beyond whatever the operator's chosen log
// sink happens to support, confirmed by this session's own enterprise-
// procurement-buyer-criteria-2026-09-22.md research finding. This
// package is additive to that line: the slog line keeps flowing to
// whatever sink it always has (since 2026-10-16 written after the
// durable append and, when that append succeeded, carrying the chain
// head as audit_seq/audit_hash, see below), and this ALSO appends one JSONL record per event to a
// dedicated, operator-configured file, specifically so it can be
// queried later without depending on the operator's own
// log-aggregation stack.
//
// Since 2026-10-16 (round-5 Tier S item 26) the file is tamper-evident:
// every appended line carries a sequence number and the SHA-256 of the
// previous line's bytes, and -- when Open was given a key -- an
// HMAC-SHA256 over its own canonical bytes. Verify walks a file and
// reports every line where that does not hold: an edited, deleted,
// reordered or forged line, a truncated tail, a line that is not JSON.
// What it cannot detect: a rewrite by the key holder (or, without a key,
// by anyone who recomputes the chain); the legacy lines before the first
// chained one other than the last, which nothing chains; and -- when the
// gateway restarts before its next append, so that resume continues from
// whatever the file says -- the deletion of the whole file, the removal
// of a tail back to a line boundary, or the rewrite of every line into
// legacy lines. Without a restart the next append's seq and prev_hash
// expose a tail cut or a legacy rewrite made in place; a deleted file is
// shown only by the stdout sink either way, because the descriptor stays
// on the unlinked inode. Which is why Append returns the chain head and
// internal/admin writes it into the slog audit line too, so the stdout
// sink keeps an independent copy, and why Open reports what it resumed
// (Resumed) so cmd/gateway can warn when a keyed trail's history carries
// no mac at all.
package auditstore

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// MaxLineBytes bounds one audit line for Open's resume scan and for
// Verify. A real line is a few hundred bytes (a handful of short field
// values), so anything past 1 MiB is a corrupted or hostile file: Verify
// reports it as a problem and stops, Open refuses the file, neither scans
// without limit. Query keeps bufio.Scanner's 64 KiB default, as it always
// has.
const MaxLineBytes = 1 << 20

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
	// Seq, PrevHash and MAC make the file tamper-evident (2026-10-16,
	// round-5 Tier S item 26). Seq is 1 for the first chained line and one
	// more for each line after it. PrevHash is the lowercase hex SHA-256
	// of the previous line's bytes -- the whole JSON line, without its
	// trailing newline, whether or not that line was itself chained -- and
	// "" for the first line of the file, so the first chained line's
	// PrevHash binds in the LAST line of a legacy, unchained prefix (the
	// earlier legacy lines are not chained to each other and cannot be
	// verified). MAC is
	// "v1,<lowercase hex HMAC-SHA256>" over the line's canonical bytes
	// (this Entry marshalled with MAC empty) under the key Open was given,
	// and absent when there was none. A line with Seq 0 predates chaining.
	// GET /admin/audit does not expose these three fields; kelvran audit
	// verify reads them from the file.
	Seq      uint64 `json:"seq,omitempty"`
	PrevHash string `json:"prev_hash,omitempty"`
	MAC      string `json:"mac,omitempty"`
}

// Head is the chain's position after an Append: the Seq the written line
// carries and the hex SHA-256 of its bytes -- the next line's PrevHash,
// and what internal/admin records on the slog audit line as audit_seq
// and audit_hash so the stdout sink holds a copy of the head that a
// truncated or deleted file cannot erase.
type Head struct {
	Seq  uint64
	Hash string
}

// Store appends Entry records to a single JSONL file, one per line, and
// answers Query calls against that same file. One Store per process per
// file: the chain state lives in memory between appends, resumed from
// the file by Open.
type Store struct {
	mu          sync.Mutex
	f           *os.File
	path        string
	key         []byte
	nextSeq     uint64
	lastHash    string
	needNewline bool
	resumed     Resumed
}

// Resumed is what Open found in the file: the lines it scanned, the last
// chained seq among them (0 when none) and whether any chained line
// carried a mac (a mac member on a legacy line does not count, exactly as
// Verify does not count it). cmd/gateway warns when a keyed store resumes
// lines without one.
type Resumed struct {
	Lines   int
	LastSeq uint64
	SawMAC  bool
}

// Resumed reports what Open found in the file before the first Append.
func (s *Store) Resumed() Resumed { return s.resumed }

// Open opens (creating if absent) path for appending. The file is never
// truncated -- every Append call adds a new line, and a restart resumes
// appending to the same durable history rather than starting fresh. key,
// when non-empty, is the HMAC-SHA256 key every appended line is signed
// with; nil means a chain without MACs. Open reads the existing file once
// to resume the chain after its last line: the next Seq follows the last
// chained line's, the next PrevHash is the hash of the last line's bytes
// whatever that line was (legacy, torn, or chained), and a last line that
// lacks its trailing newline is terminated before anything is appended
// after it, so the new line never glues onto a torn one.
func Open(path string, key []byte) (*Store, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("auditstore: opening %q: %w", path, err)
	}
	s := &Store{f: f, path: path, key: append([]byte(nil), key...), nextSeq: 1}
	if err := s.resume(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

// resume positions the chain after the file's last line; see Open.
func (s *Store) resume() error {
	r, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("auditstore: reading %q to resume the chain: %w", s.path, err)
	}
	defer func() { _ = r.Close() }()
	var last []byte
	var lastChainedSeq uint64
	haveLast, terminated := false, true
	err = eachLine(r, func(raw []byte, term bool) error {
		last = append(last[:0], raw...)
		haveLast, terminated = true, term
		s.resumed.Lines++
		var e Entry
		if json.Unmarshal(raw, &e) == nil {
			if e.Seq > 0 {
				lastChainedSeq = e.Seq
			}
			if e.Seq > 0 && e.MAC != "" {
				s.resumed.SawMAC = true
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("auditstore: reading %q to resume the chain: %w", s.path, err)
	}
	s.resumed.LastSeq = lastChainedSeq
	if !haveLast {
		return nil
	}
	s.lastHash = lineHash(last)
	if lastChainedSeq > 0 {
		s.nextSeq = lastChainedSeq + 1
	}
	s.needNewline = !terminated
	return nil
}

// Append writes one Entry, built from msg and args (the identical
// alternating key/value pairs admin's own auditLogger.Info already
// receives -- this function has no opinion on what they mean, only on
// storing them), chained to the previous line and signed when the Store
// has a key, and returns the chain head the line now is. An args slice
// with an odd length, or a non-string key, is a caller bug elsewhere in
// this codebase (every real call site in internal/admin uses literal
// string keys) -- rather than panicking or silently dropping the whole
// entry, the offending pair is recorded under a synthesized key so the
// rest of the entry is never lost.
func (s *Store) Append(msg string, args ...any) (Head, error) {
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

	s.mu.Lock()
	defer s.mu.Unlock()
	e := Entry{Time: time.Now(), Msg: msg, Fields: fields, Seq: s.nextSeq, PrevHash: s.lastHash}
	line, err := json.Marshal(e)
	if err != nil {
		return Head{}, fmt.Errorf("auditstore: marshaling entry: %w", err)
	}
	if len(s.key) > 0 {
		e.MAC = macOf(s.key, line)
		if line, err = json.Marshal(e); err != nil {
			return Head{}, fmt.Errorf("auditstore: marshaling signed entry: %w", err)
		}
	}
	buf := make([]byte, 0, len(line)+2)
	if s.needNewline {
		buf = append(buf, '\n')
	}
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err := s.f.Write(buf); err != nil {
		return Head{}, fmt.Errorf("auditstore: writing entry: %w", err)
	}
	s.needNewline = false
	s.lastHash = lineHash(line)
	head := Head{Seq: e.Seq, Hash: s.lastHash}
	s.nextSeq++
	return head, nil
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

// lineHash is the chain's hash of one line's bytes (no trailing newline).
func lineHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// macOf is the "v1,<hex>" HMAC-SHA256 of canonical under key.
func macOf(key, canonical []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(canonical)
	return "v1," + hex.EncodeToString(m.Sum(nil))
}

// Problem is one thing Verify found wrong, at a 1-based line number (0
// for a problem with the whole file).
// Kind is one of: truncated_tail (the last line has no trailing newline),
// malformed (not a JSON record), legacy_after_chain (a line without seq
// after chained lines: a downgraded gateway, or not the gateway), seq (a
// gap, repeat or regression), prev_hash (the link to the previous line's
// bytes does not hold), non_canonical (the line's bytes are not the
// gateway's own encoding of its content: a key added, duplicated or
// re-spaced, which a mac over the parsed content cannot see), mac_missing
// (no mac on a chained line although a key was given, or after signing
// began), mac_invalid (the mac does not verify under the key),
// no_signed_lines (a key was given but no line carries a mac: the trail
// predates the key, or every line was rewritten as legacy; line 0),
// line_too_long (a line past MaxLineBytes; Verify stops there).
type Problem struct {
	Line   int    `json:"line"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Report is Verify's result: counts, the chain's range and head, every
// per-line Problem in file order, then any whole-file Problem (Line 0).
// Problems is never nil.
type Report struct {
	Lines        int
	LegacyLines  int
	ChainedLines int
	SignedLines  int
	// UnsignedChainedLines counts chained lines without a mac, with or
	// without a key: a trail chained before a key was configured, or one
	// whose macs were removed.
	UnsignedChainedLines int
	UncheckedMACs        int
	FirstSeq             uint64
	LastSeq              uint64
	HeadHash             string
	Problems             []Problem
}

// OK reports whether Verify found nothing wrong.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// VerifyOptions tunes VerifyWithOptions. The zero value checks the chain
// alone and counts the macs without checking them.
type VerifyOptions struct {
	// Key verifies every mac. Nil counts signed lines as UncheckedMACs.
	Key []byte
	// AllowUnsignedPrefix accepts chained lines without a mac that precede
	// the first signed line: a file chained before the operator configured
	// a key. Without it a Key makes every such line a mac_missing problem,
	// so a tamperer who strips every mac and recomputes the chain cannot
	// pass the file off as one that was never signed. A line without a
	// mac after signing began is a problem either way.
	AllowUnsignedPrefix bool
}

// Verify is VerifyWithOptions with the key alone: with a key, every
// chained line must carry a mac.
func Verify(r io.Reader, key []byte) (Report, error) {
	return VerifyWithOptions(r, VerifyOptions{Key: key})
}

// VerifyWithOptions walks every line of r and checks the chain Append
// writes: a legacy prefix (lines without seq) is allowed only before the
// first chained line; chained lines carry seq 1, 2, 3, ... with no gap;
// each line's prev_hash is the SHA-256 of the previous line's bytes
// whatever that line was; each chained line's bytes are the gateway's own
// encoding of its content; with a key every chained line carries a mac
// (see AllowUnsignedPrefix) and each mac verifies under it -- without a
// key the macs are counted as UncheckedMACs instead and the chain alone
// is checked, though once a line carries a mac every later chained line
// still must. Unlike Query, it never skips a line it cannot parse: that is
// a finding. The returned error is for reading r itself, never for what
// the bytes say.
func VerifyWithOptions(r io.Reader, opts VerifyOptions) (Report, error) {
	key := opts.Key
	rep := Report{Problems: []Problem{}}
	var prevRaw []byte
	havePrev, chainStarted, signingStarted := false, false, false
	var expectSeq uint64
	n := 0
	note := func(kind, detail string) {
		rep.Problems = append(rep.Problems, Problem{Line: n, Kind: kind, Detail: detail})
	}
	err := eachLine(r, func(raw []byte, terminated bool) error {
		n++
		rep.Lines = n
		defer func() {
			prevRaw = append(prevRaw[:0], raw...)
			havePrev = true
		}()
		if !terminated {
			note("truncated_tail", "the last line has no trailing newline: a write was cut short or the file was truncated")
			return nil
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			note("malformed", "not a JSON audit record: "+err.Error())
			return nil
		}
		if e.Seq == 0 {
			if chainStarted {
				note("legacy_after_chain", "a line without seq after the chain began: written by a gateway older than 2026-10-16 (a downgrade) or not by the gateway")
			} else {
				rep.LegacyLines++
			}
			return nil
		}
		rep.ChainedLines++
		if !chainStarted {
			chainStarted = true
			rep.FirstSeq = e.Seq
			if e.Seq != 1 {
				note("seq", fmt.Sprintf("the first chained line has seq %d, want 1", e.Seq))
			}
		} else if e.Seq != expectSeq {
			note("seq", fmt.Sprintf("seq %d, want %d", e.Seq, expectSeq))
		}
		expectSeq = e.Seq + 1
		rep.LastSeq = e.Seq
		wantPrev := ""
		if havePrev {
			wantPrev = lineHash(prevRaw)
		}
		if e.PrevHash != wantPrev {
			note("prev_hash", "prev_hash does not match the previous line's SHA-256: that line was changed, removed or moved")
		}
		rep.HeadHash = lineHash(raw)
		if canonical, err := json.Marshal(e); err != nil || !bytes.Equal(canonical, raw) {
			note("non_canonical", "the line's bytes are not the gateway's own encoding of its content: a key was added, duplicated or re-spaced")
		}
		switch e.MAC {
		case "":
			rep.UnsignedChainedLines++
			switch {
			case signingStarted:
				note("mac_missing", "no mac after signing began: not written by the gateway with its key")
			case len(key) > 0 && !opts.AllowUnsignedPrefix:
				note("mac_missing", "no mac on a chained line although a key was given: the trail predates the key, or every mac was removed")
			}
		default:
			rep.SignedLines++
			signingStarted = true
			if len(key) == 0 {
				rep.UncheckedMACs++
				return nil
			}
			unsigned := e
			unsigned.MAC = ""
			canonical, err := json.Marshal(unsigned)
			if err != nil {
				note("mac_invalid", "the line cannot be re-marshalled for verification: "+err.Error())
				return nil
			}
			if !strings.HasPrefix(e.MAC, "v1,") || !hmac.Equal([]byte(e.MAC), []byte(macOf(key, canonical))) {
				note("mac_invalid", "mac does not verify under the given key: the line was changed or signed with another key")
			}
		}
		return nil
	})
	if errors.Is(err, errLineTooLong) {
		n++
		rep.Lines = n
		note("line_too_long", fmt.Sprintf("line exceeds %d bytes; verification stopped here", MaxLineBytes))
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	// A key is the operator's claim that the file is signed. With nothing
	// signed there is nothing that claim can be checked against, which is
	// exactly what a tamperer who rewrote every line as legacy (no seq, no
	// prev_hash, no mac) leaves behind -- so the claim fails. An empty file
	// is the whole-file-deletion class, which only the stdout sink shows.
	if len(key) > 0 && rep.Lines > 0 && rep.SignedLines == 0 {
		n = 0
		note("no_signed_lines", "a key was given but no line carries a mac: the trail predates the key, or every line was rewritten without seq, prev_hash and mac; verify without a key to check the chain alone, and start a new file when you introduce a key")
	}
	return rep, nil
}

var errLineTooLong = errors.New("auditstore: line exceeds MaxLineBytes")

// eachLine calls fn for every line of r with the line's bytes (no
// trailing newline) and whether a newline terminated it; the final line
// of a file that does not end in a newline is delivered with terminated
// false. A line longer than MaxLineBytes ends the walk with errLineTooLong
// before it is read whole.
func eachLine(r io.Reader, fn func(raw []byte, terminated bool) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxLineBytes+1 {
			return errLineTooLong
		}
		switch {
		case err == nil:
			if e := fn(line[:len(line)-1], true); e != nil {
				return e
			}
			line = line[:0]
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) > 0 {
				return fn(line, false)
			}
			return nil
		default:
			return err
		}
	}
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
// file) is skipped, not fatal to the whole query -- Verify is the reader
// that reports it. Chained and legacy lines are returned alike; the
// integrity fields come back on the Entry and GET /admin/audit drops them.
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
