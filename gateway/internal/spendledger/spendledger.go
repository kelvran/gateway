// Package spendledger is the bounded spend ledger of
// docs/rfcs/2026-10-09-gateway-attribution-and-spend-ledger.md (item 13d,
// owner gates G27-G29): hourly UTC-floored buckets keyed by (virtual key,
// served model, client tool, session) holding nano-USD spend and request
// counts, kept for a retention window, with two caps per key -- distinct
// sessions and distinct rows -- whose overflow folds into the `other`
// session rather than growing. The package is a leaf: it imports nothing
// from this project. Backends live beside it (memory here, bbolt and Redis
// in sub-packages) and share the contract in ledgertest.
package spendledger

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Group-by dimensions of Query; the admin route exposes exactly these.
const (
	GroupByKey     = "key"
	GroupByModel   = "model"
	GroupByTool    = "tool"
	GroupBySession = "session"
)

// OtherSession is the fold target: a key's sessions beyond
// MaxSessionsPerKey and its rows beyond MaxRowsPerKey are recorded under it.
// Like the empty session it occupies no slot in either cap (see
// CountsTowardsCaps), so a fold can never itself be refused. It is never
// erasable on its own -- only DeleteKey or retention removes it.
const OtherSession = "other"

// MaxSessionBytes bounds a session identifier (the Claude Code session id
// grammar of item 13a; the erase route of slice 4 accepts the same shape).
const MaxSessionBytes = 128

// MaxDimensionBytes bounds the key, model and tool of an Entry (the admin
// API's identifier bound). The callers' vocabularies are far smaller -- the
// served model comes from the deployment table and the client tool from the
// thirteen-value normalisation of item 13a -- so this is defence in depth
// against a future caller passing a request-controlled string.
const MaxDimensionBytes = 256

// Defaults per RFC-2 and G29.
const (
	DefaultRetention         = 168 * time.Hour
	DefaultMaxSessionsPerKey = 2000
	DefaultMaxRowsPerKey     = 50000
)

// Entry is one billed request's contribution.
type Entry struct {
	Key, Model, Tool, Session string
	At                        time.Time
	SpendNanoUSD, Requests    int64
}

// Query selects the hour buckets whose start lies in [HourBucket(Since),
// Until) -- so a mid-hour Until takes its whole bucket and a zero Until
// selects nothing -- optionally for one key, aggregated by GroupBy.
type Query struct {
	GroupBy, Key string
	Since, Until time.Time
}

// Row is one aggregate: the dimension the GroupBy kept, plus Key when the
// query names one key or groups by session (others empty), and the sums.
type Row struct {
	Key, Model, Tool, Session string
	SpendNanoUSD, Requests    int64
}

// Options bound a ledger; zero values take the defaults.
//
// MaxSessionsPerKey caps the distinct session names a key keeps in the
// retention window; MaxRowsPerKey caps the stored cells of a key that carry
// a session name -- a (model, tool, session) row inside one hour bucket, so
// the same row in a later hour takes a new slot. Both count only sessions for
// which CountsTowardsCaps is true: rows without a session and rows under
// OtherSession are bounded by the model and tool vocabularies per hour, not
// by traffic, so they take no slot and are never folded. Sweep frees the
// slots of the hours it drops.
type Options struct {
	Retention         time.Duration
	MaxSessionsPerKey int
	MaxRowsPerKey     int
}

// WithDefaults fills zero fields with the RFC-2 / G29 defaults.
func (o Options) WithDefaults() Options {
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.MaxSessionsPerKey <= 0 {
		o.MaxSessionsPerKey = DefaultMaxSessionsPerKey
	}
	if o.MaxRowsPerKey <= 0 {
		o.MaxRowsPerKey = DefaultMaxRowsPerKey
	}
	return o
}

// Ledger is the contract every backend meets (ledgertest.Run).
type Ledger interface {
	// Record adds one entry to its hour bucket, folding per the caps.
	Record(ctx context.Context, e Entry) error
	// Query aggregates the window; rows come sorted by spend, highest first.
	Query(ctx context.Context, q Query) ([]Row, error)
	// DeleteKey removes every row of key in every stored hour bucket
	// (idempotent).
	DeleteKey(ctx context.Context, key string) error
	// DeleteSession removes every row of session across keys and hours and
	// returns how many rows went; "" and OtherSession are refused.
	DeleteSession(ctx context.Context, session string) (int, error)
	// Sweep drops every hour bucket that starts before
	// HourBucket(now) - Retention and keeps the bucket at exactly that
	// cutoff, so a Query whose Since is now - Retention still reads a
	// complete window; it then rebuilds the derived per-key session and
	// row sets.
	Sweep(ctx context.Context, now time.Time) error
	Close() error
}

// The leaf's sentinels. ErrSessionReserved and ErrSessionNotIdentifier are
// wrapped in ErrInvalidEntry when Record refuses an entry for its session.
var (
	ErrInvalidEntry         = errors.New("spendledger: invalid entry")
	ErrInvalidGroupBy       = errors.New("spendledger: unknown group_by")
	ErrSessionNotErasable   = errors.New("spendledger: session cannot be erased individually")
	ErrSessionNotIdentifier = errors.New("spendledger: session is not an identifier")
	ErrSessionReserved      = errors.New("spendledger: session name is reserved for the fold target")
)

// HourBucket floors t to its UTC hour, the ledger's unit.
func HourBucket(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }

// IsSessionIdentifier reports whether s is 1..MaxSessionBytes bytes of
// [A-Za-z0-9._:-] -- the only sessions a ledger stores by name, so the
// stores' delimiters (`|`, `#`) never collide and the erase route's
// argument has the same shape.
func IsSessionIdentifier(s string) bool {
	if s == "" || len(s) > MaxSessionBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateGroupBy accepts the four dimensions.
func ValidateGroupBy(g string) error {
	switch g {
	case GroupByKey, GroupByModel, GroupByTool, GroupBySession:
		return nil
	}
	return fmt.Errorf("%w %q (want key, model, tool or session)", ErrInvalidGroupBy, g)
}

// ValidateEntry refuses an empty key, a key, model or tool over
// MaxDimensionBytes, a zero time, a negative spend, fewer
// than one request (an entry is a billed request; spend may be zero on an
// unpriced deployment, the request count may not), the reserved session
// name OtherSession (only a fold writes it, so a wire value equal to it
// cannot merge into the fold target -- the write hook maps it to "") and a
// non-empty session that is not an identifier.
func ValidateEntry(e Entry) error {
	switch {
	case e.Key == "":
		return fmt.Errorf("%w: empty key", ErrInvalidEntry)
	case len(e.Key) > MaxDimensionBytes || len(e.Model) > MaxDimensionBytes || len(e.Tool) > MaxDimensionBytes:
		return fmt.Errorf("%w: a dimension exceeds %d bytes", ErrInvalidEntry, MaxDimensionBytes)
	case e.At.IsZero():
		return fmt.Errorf("%w: zero time", ErrInvalidEntry)
	case e.SpendNanoUSD < 0:
		return fmt.Errorf("%w: negative spend", ErrInvalidEntry)
	case e.Requests < 1:
		return fmt.Errorf("%w: fewer than one request", ErrInvalidEntry)
	case e.Session == OtherSession:
		return fmt.Errorf("%w: %w", ErrInvalidEntry, ErrSessionReserved)
	case e.Session != "" && !IsSessionIdentifier(e.Session):
		return fmt.Errorf("%w: %w", ErrInvalidEntry, ErrSessionNotIdentifier)
	}
	return nil
}

// CellKey identifies one row inside an hour bucket.
type CellKey struct{ Key, Model, Tool, Session string }

// Encode renders the cell key as `key|model|tool|session` with every part
// query-escaped, so a `|` inside a model name cannot forge a different row.
func (c CellKey) Encode() string {
	return url.QueryEscape(c.Key) + "|" + url.QueryEscape(c.Model) + "|" + url.QueryEscape(c.Tool) + "|" + url.QueryEscape(c.Session)
}

// cellKeyParts names the Encode positions, for DecodeCellKey's errors.
var cellKeyParts = [4]string{"key", "model", "tool", "session"}

// DecodeCellKey is Encode's inverse. Its errors name the part that failed,
// never the key itself: a cell key may carry a session identifier and these
// errors reach log lines.
func DecodeCellKey(s string) (CellKey, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 4 {
		return CellKey{}, fmt.Errorf("spendledger: malformed cell key: %d parts, want 4", len(parts))
	}
	var out [4]string
	for i, p := range parts {
		v, err := url.QueryUnescape(p)
		if err != nil {
			return CellKey{}, fmt.Errorf("spendledger: malformed cell key: %s part: %w", cellKeyParts[i], err)
		}
		out[i] = v
	}
	return CellKey{Key: out[0], Model: out[1], Tool: out[2], Session: out[3]}, nil
}

// Cell is one stored row: its key and sums, with the hour it belongs to.
type Cell struct {
	Hour time.Time
	CellKey
	SpendNanoUSD, Requests int64
}

// FoldSession applies the session cap: a session is kept by name when it is
// empty, already live for the key, or when the key has room; otherwise it
// becomes OtherSession. "" and OtherSession (which ValidateEntry keeps off
// the input side; the helper stays total) never count towards the cap.
func FoldSession(session string, live map[string]struct{}, maxSessions int) string {
	if session == "" || session == OtherSession {
		return session
	}
	if _, ok := live[session]; ok {
		return session
	}
	if len(live) >= maxSessions {
		return OtherSession
	}
	return session
}

// CountsTowardsCaps reports whether a session name occupies a slot in the
// per-key session set and whether its rows count towards MaxRowsPerKey:
// true for an identifier, false for the empty session and OtherSession.
func CountsTowardsCaps(session string) bool { return session != "" && session != OtherSession }

// InWindow reports whether an hour bucket is selected by q: buckets whose
// start is at or after HourBucket(q.Since) and before q.Until (a zero
// q.Until selects nothing; a zero q.Since selects from the first bucket).
func InWindow(hour time.Time, q Query) bool {
	return !hour.Before(HourBucket(q.Since)) && hour.Before(q.Until)
}

// Aggregate groups the cells by q.GroupBy (the key is always kept when q.Key
// narrows the query or when grouping by session, which is per key) and
// sorts the rows by spend, highest first, then by identity for a stable
// order.
func Aggregate(cells []Cell, q Query) ([]Row, error) {
	if err := ValidateGroupBy(q.GroupBy); err != nil {
		return nil, err
	}
	acc := map[CellKey]*Row{}
	for _, c := range cells {
		if q.Key != "" && c.Key != q.Key {
			continue
		}
		if !InWindow(c.Hour, q) {
			continue
		}
		id := CellKey{}
		if q.Key != "" || q.GroupBy == GroupByKey || q.GroupBy == GroupBySession {
			id.Key = c.Key
		}
		switch q.GroupBy {
		case GroupByModel:
			id.Model = c.Model
		case GroupByTool:
			id.Tool = c.Tool
		case GroupBySession:
			id.Session = c.Session
		}
		r, ok := acc[id]
		if !ok {
			r = &Row{Key: id.Key, Model: id.Model, Tool: id.Tool, Session: id.Session}
			acc[id] = r
		}
		r.SpendNanoUSD += c.SpendNanoUSD
		r.Requests += c.Requests
	}
	rows := make([]Row, 0, len(acc))
	for _, r := range acc {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SpendNanoUSD != rows[j].SpendNanoUSD {
			return rows[i].SpendNanoUSD > rows[j].SpendNanoUSD
		}
		return lessIdentity(rows[i], rows[j])
	})
	return rows, nil
}

// lessIdentity orders two rows of equal spend by (key, model, tool, session),
// field by field, so no two distinct rows ever compare equal.
func lessIdentity(a, b Row) bool {
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	if a.Model != b.Model {
		return a.Model < b.Model
	}
	if a.Tool != b.Tool {
		return a.Tool < b.Tool
	}
	return a.Session < b.Session
}

// Memory is the in-process ledger, intended for the case where neither
// ledger.persist_path nor budget.redis_addr is configured (the wiring lands
// in slice 3): bounded like the others, lost on restart.
type Memory struct {
	mu    sync.Mutex
	opts  Options
	hours map[int64]map[CellKey]*cellSums // hour unix -> rows
	// derived per-key state over the cells that take a slot (CountsTowardsCaps),
	// rebuilt by Sweep and adjusted by the deletes
	sessions map[string]map[string]struct{}
	cells    map[string]map[cellRef]struct{}
}

type cellSums struct{ spend, requests int64 }

// cellRef identifies one stored cell: a row inside one hour bucket. The row
// cap counts cells, so the same row in a later hour takes a new slot.
type cellRef struct {
	hour int64
	CellKey
}

// NewMemory returns an empty in-memory ledger.
func NewMemory(opts Options) *Memory {
	return &Memory{opts: opts.WithDefaults(), hours: map[int64]map[CellKey]*cellSums{}, sessions: map[string]map[string]struct{}{}, cells: map[string]map[cellRef]struct{}{}}
}

// Record implements Ledger.
func (m *Memory) Record(_ context.Context, e Entry) error {
	if err := ValidateEntry(e); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	live := m.sessions[e.Key]
	if live == nil {
		live = map[string]struct{}{}
		m.sessions[e.Key] = live
	}
	slots := m.cells[e.Key]
	if slots == nil {
		slots = map[cellRef]struct{}{}
		m.cells[e.Key] = slots
	}
	hour := HourBucket(e.At)
	ck := CellKey{Key: e.Key, Model: e.Model, Tool: e.Tool, Session: FoldSession(e.Session, live, m.opts.MaxSessionsPerKey)}
	counts := CountsTowardsCaps(ck.Session)
	if _, exists := slots[cellRef{hour.Unix(), ck}]; counts && !exists && len(slots) >= m.opts.MaxRowsPerKey {
		ck.Session = OtherSession
		counts = false
	}
	bucket := m.hours[hour.Unix()]
	if bucket == nil {
		bucket = map[CellKey]*cellSums{}
		m.hours[hour.Unix()] = bucket
	}
	c := bucket[ck]
	if c == nil {
		c = &cellSums{}
		bucket[ck] = c
	}
	c.spend += e.SpendNanoUSD
	c.requests += e.Requests
	if counts {
		slots[cellRef{hour.Unix(), ck}] = struct{}{}
		live[ck.Session] = struct{}{}
	}
	return nil
}

// cellsIn returns the stored cells of the hours q selects.
func (m *Memory) cellsIn(q Query) []Cell {
	var out []Cell
	for h, bucket := range m.hours {
		hour := time.Unix(h, 0).UTC()
		if !InWindow(hour, q) {
			continue
		}
		for ck, c := range bucket {
			out = append(out, Cell{Hour: hour, CellKey: ck, SpendNanoUSD: c.spend, Requests: c.requests})
		}
	}
	return out
}

// Query implements Ledger.
func (m *Memory) Query(_ context.Context, q Query) ([]Row, error) {
	m.mu.Lock()
	cells := m.cellsIn(q)
	m.mu.Unlock()
	return Aggregate(cells, q)
}

// DeleteKey implements Ledger.
func (m *Memory) DeleteKey(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, bucket := range m.hours {
		for ck := range bucket {
			if ck.Key == key {
				delete(bucket, ck)
			}
		}
		if len(bucket) == 0 {
			delete(m.hours, h)
		}
	}
	delete(m.sessions, key)
	delete(m.cells, key)
	return nil
}

// DeleteSession implements Ledger.
func (m *Memory) DeleteSession(_ context.Context, session string) (int, error) {
	if session == "" || session == OtherSession {
		return 0, ErrSessionNotErasable
	}
	if !IsSessionIdentifier(session) {
		return 0, ErrSessionNotIdentifier
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for h, bucket := range m.hours {
		for ck := range bucket {
			if ck.Session == session {
				delete(bucket, ck)
				n++
				delete(m.cells[ck.Key], cellRef{h, ck})
			}
		}
		if len(bucket) == 0 {
			delete(m.hours, h)
		}
	}
	for _, live := range m.sessions {
		delete(live, session)
	}
	return n, nil
}

// Sweep implements Ledger.
func (m *Memory) Sweep(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := HourBucket(now).Add(-m.opts.Retention)
	for h := range m.hours {
		if time.Unix(h, 0).UTC().Before(cutoff) {
			delete(m.hours, h)
		}
	}
	m.sessions = map[string]map[string]struct{}{}
	m.cells = map[string]map[cellRef]struct{}{}
	for h, bucket := range m.hours {
		for ck := range bucket {
			if !CountsTowardsCaps(ck.Session) {
				continue
			}
			if m.cells[ck.Key] == nil {
				m.cells[ck.Key] = map[cellRef]struct{}{}
				m.sessions[ck.Key] = map[string]struct{}{}
			}
			m.cells[ck.Key][cellRef{h, ck}] = struct{}{}
			m.sessions[ck.Key][ck.Session] = struct{}{}
		}
	}
	return nil
}

// Close implements Ledger; nothing to release.
func (m *Memory) Close() error { return nil }
