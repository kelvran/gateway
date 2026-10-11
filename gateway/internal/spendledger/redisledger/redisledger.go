// Package redisledger is the Redis backend of the spend ledger (item 13d;
// the RFC's `spendledger/redis`, renamed like redisbudget and redisstore
// because a package called `redis` would shadow the client import), used
// when `budget.redis_addr` is set (the wiring lands in slice 3): one hash
// per (UTC hour, virtual key), `ledger:<hourUnix>:<QueryEscape(key)>`,
// whose fields are `<model>|<tool>|<session>` (nano-USD) and the same name
// with `#n` appended (requests), every part query-escaped like the leaf's
// cell keys. The per-key session set (`ledger:sessions:<key>`) and counting-
// cell count (`ledger:cells:<key>`) are derived state, as in the bbolt
// backend; the difference is that here the fold decision -- is the session
// live, is there room, does the cell exist -- runs inside Redis in one Lua
// script, so every gateway replica writing the same key shares one view of
// the caps and no interleaving can admit a slot twice.
//
// Retention is enforced the same way as the other backends, by Sweep against
// the caller's clock (whole hour hashes whose hour starts before
// HourBucket(now) - Retention go; the hash at the cutoff stays); each hash
// also carries a TTL of Retention + 1 h from its first write as a backstop
// for a sweeper that never runs. The slack moves expiry to no earlier than
// hour + Retention + 1 h, the first instant a Sweep may drop the hash, so the
// TTL never removes a hash a query inside the retention window could still
// need; which of the two removes it is immaterial, because the next Sweep
// rebuilds the derived state either way (this assumes Entry.At is not ahead
// of the Redis server's clock by more than the slack; the write hook passes
// now). The derived state carries no TTL and is rebuilt by every Sweep and
// DeleteSession from the hashes they have just read, and removed with the
// key by DeleteKey, so it is eventually exact: the Records that land between
// a rebuild's read and its write are missing from the derived state until
// the next hourly Sweep -- the caps can over-admit by that many, and a
// session first seen in that window can fold on its next write although it
// holds a named cell.
//
// Every read is one SCAN of the store's hour hashes (bounded by hours x
// keys; RFC-2 calls it not a hot path) followed by pipelined HGETALLs of the
// hashes in the window, chunkSize per pipeline. A key under the prefix that
// is not a hash is foreign: reads skip it, Sweep leaves it in place (until
// its hour name expires, when it is dropped like any hash), nothing writes
// to it -- so the `ledger:` prefix is the store's alone; an operator should
// create nothing under it. Single-node Redis, like the budget: the script touches three keys
// of one virtual key, so a cluster would need hash tags to co-locate them.
//
// Like internal/budget/redisbudget and internal/identity/redisstore, Open
// never fails on an unreachable address (dialing is lazy) so the gateway can
// start while Redis is briefly down. Like identity/redisstore and unlike
// redisbudget, this package imports the leaf: the Ledger interface takes the
// leaf's own Entry and Query types, so there is no primitive-only shape to
// satisfy structurally.
//
// Error strings name hours and keys, never a field or a session: a field
// carries a session identifier and these errors reach log lines.
package redisledger

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
)

const (
	keyPrefix       = "ledger:"
	sessionsSegment = "sessions" // ledger:sessions:<key>  -- set of live named sessions
	cellsSegment    = "cells"    // ledger:cells:<key>     -- count of live counting cells
	requestsSuffix  = "#n"       // `#` is query-escaped inside a field, so the suffix is unambiguous
	// ttlSlack is how far the TTL backstop outlives the retention. A write
	// lands inside its own hour, so a hash created at hour h expires between
	// h + Retention + 1 h and h + Retention + 2 h -- never before the first
	// instant a Sweep may drop it, so never while a query inside the
	// retention window could still need it.
	ttlSlack = time.Hour
	// scanCount is the SCAN hint; the store holds at most hours x keys hashes.
	scanCount = 500
)

// chunkSize bounds one pipeline of HGETALLs and one DEL: a wide window over
// many keys, or a store left unswept for days, is read and dropped in
// bounded commands rather than one with hundreds of thousands of arguments.
// A variable so the tests can shrink it and cross the boundaries.
var chunkSize = 1000

// recordLuaSrc applies both caps and writes one entry atomically.
//
// KEYS[1] the hour hash, KEYS[2] the key's session set, KEYS[3] the key's
// counting-cell count. ARGV: 1 the field, 2 the requests field, 3 the spend
// in nano-USD, 4 the requests, 5 the session ("" when none), 6 "1" when the
// session takes a slot (an identifier; the leaf refuses `other` as input),
// 7 max sessions, 8 max cells, 9 the TTL in ms, 10 and 11 the `other` field
// pair the entry folds into. Returns 1 when it folded, else 0 -- unused by
// Record today, kept for a fold-rate counter later.
//
// Order matters and mirrors the leaf: a session beyond the cap folds and
// then takes no cell slot; a live session's new cell beyond the cell cap
// folds likewise. Amounts cross as integer strings for HINCRBY, never as
// Lua numbers (a float64 double), per redisbudget's own precision note.
const recordLuaSrc = `
local field, nfield = ARGV[1], ARGV[2]
local session = ARGV[5]
local counts = ARGV[6] == '1'
local folded = 0
if counts and redis.call('SISMEMBER', KEYS[2], session) == 0 and redis.call('SCARD', KEYS[2]) >= tonumber(ARGV[7]) then
  field, nfield, counts, folded = ARGV[10], ARGV[11], false, 1
end
-- GET on a missing key is false in Lua; or '0' reads an absent count as 0
if counts and redis.call('HEXISTS', KEYS[1], field) == 0 and tonumber(redis.call('GET', KEYS[3]) or '0') >= tonumber(ARGV[8]) then
  field, nfield, counts, folded = ARGV[10], ARGV[11], false, 1
end
local fresh = redis.call('EXISTS', KEYS[1]) == 0
local new = counts and redis.call('HEXISTS', KEYS[1], field) == 0
redis.call('HINCRBY', KEYS[1], field, ARGV[3])
redis.call('HINCRBY', KEYS[1], nfield, ARGV[4])
if fresh then
  redis.call('PEXPIRE', KEYS[1], ARGV[9])
end
if counts then
  redis.call('SADD', KEYS[2], session)
  if new then
    redis.call('INCR', KEYS[3])
  end
end
return folded
`

// Store is a Redis-backed spendledger.Ledger. The zero value is not usable;
// construct with Open.
type Store struct {
	client *redis.Client
	opts   spendledger.Options
	record *redis.Script
}

// Open constructs a Store against the Redis server described by ropts with
// the ledger bounds in lopts (zero fields take the defaults). It never fails
// on an unreachable address; the first operation reports that.
func Open(ropts redis.Options, lopts spendledger.Options) (*Store, error) {
	return &Store{client: redis.NewClient(&ropts), opts: lopts.WithDefaults(), record: redis.NewScript(recordLuaSrc)}, nil
}

// Close closes the underlying Redis client.
func (s *Store) Close() error { return s.client.Close() }

func hourName(hour time.Time) string { return strconv.FormatInt(hour.Unix(), 10) }

// hashKey, sessionsKey and cellsKey percent-encode the key (url.QueryEscape
// encodes every ':' and every glob metacharacter), so no key can collide
// with another namespace or escape a SCAN pattern.
func hashKey(hour time.Time, key string) string {
	return keyPrefix + hourName(hour) + ":" + url.QueryEscape(key)
}
func sessionsKey(key string) string { return keyPrefix + sessionsSegment + ":" + url.QueryEscape(key) }
func cellsKey(key string) string    { return keyPrefix + cellsSegment + ":" + url.QueryEscape(key) }

// fieldOf renders a cell's hash field: the leaf's cell key without its key
// part.
func fieldOf(ck spendledger.CellKey) string {
	return url.QueryEscape(ck.Model) + "|" + url.QueryEscape(ck.Tool) + "|" + url.QueryEscape(ck.Session)
}

// decodeField is fieldOf's inverse for a hash of key; it reuses the leaf's
// decoder (whose errors name the part, never the text).
func decodeField(key, field string) (spendledger.CellKey, error) {
	return spendledger.DecodeCellKey(url.QueryEscape(key) + "|" + field)
}

// hashRef is one hour hash the store owns.
type hashRef struct {
	name string
	hour time.Time
	key  string
}

// parseHashKey recognises the store's hour hashes; the derived namespaces
// and any foreign key in a shared Redis are not ours (ok false).
func parseHashKey(name string) (hashRef, bool) {
	rest, found := strings.CutPrefix(name, keyPrefix)
	if !found {
		return hashRef{}, false
	}
	hourPart, escKey, found := strings.Cut(rest, ":")
	if !found || strings.Contains(escKey, ":") {
		return hashRef{}, false
	}
	n, err := strconv.ParseInt(hourPart, 10, 64)
	if err != nil {
		return hashRef{}, false
	}
	key, err := url.QueryUnescape(escKey)
	if err != nil {
		return hashRef{}, false
	}
	return hashRef{name: name, hour: time.Unix(n, 0).UTC(), key: key}, true
}

// scanHashes lists the hour hashes matching the glob (deduplicated: SCAN
// may repeat a key), optionally only those of one key.
func (s *Store) scanHashes(ctx context.Context, match, onlyKey string) ([]hashRef, error) {
	seen := map[string]struct{}{}
	var out []hashRef
	iter := s.client.Scan(ctx, 0, match, scanCount).Iterator()
	for iter.Next(ctx) {
		name := iter.Val()
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		ref, ok := parseHashKey(name)
		if !ok || (onlyKey != "" && ref.key != onlyKey) {
			continue
		}
		out = append(out, ref)
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("redisledger: scanning hour hashes: %w", err)
	}
	return out, nil
}

// keyHashes lists one key's hour hashes.
func (s *Store) keyHashes(ctx context.Context, key string) ([]hashRef, error) {
	return s.scanHashes(ctx, keyPrefix+"*:"+url.QueryEscape(key), key)
}

// Record implements spendledger.Ledger.
func (s *Store) Record(ctx context.Context, e spendledger.Entry) error {
	if err := spendledger.ValidateEntry(e); err != nil {
		return err
	}
	hour := spendledger.HourBucket(e.At)
	ck := spendledger.CellKey{Key: e.Key, Model: e.Model, Tool: e.Tool, Session: e.Session}
	other := ck
	other.Session = spendledger.OtherSession
	counts := "0"
	if spendledger.CountsTowardsCaps(e.Session) {
		counts = "1"
	}
	err := s.record.Run(ctx, s.client,
		[]string{hashKey(hour, e.Key), sessionsKey(e.Key), cellsKey(e.Key)},
		fieldOf(ck), fieldOf(ck)+requestsSuffix, e.SpendNanoUSD, e.Requests, e.Session, counts,
		s.opts.MaxSessionsPerKey, s.opts.MaxRowsPerKey, (s.opts.Retention + ttlSlack).Milliseconds(),
		fieldOf(other), fieldOf(other)+requestsSuffix,
	).Err()
	if err != nil {
		return fmt.Errorf("redisledger: recording hour %s for key %q: %w", hourName(hour), e.Key, err)
	}
	return nil
}

// fieldPair is one cell's two hash fields, read back.
type fieldPair struct {
	spend, requests string
	hasSpend        bool
}

// pairs groups a hash's fields by cell: `f` carries the spend, `f#n` the
// requests. It does not decode; callers decide what an undecodable or
// non-integer pair means (reads fail closed, Sweep removes it).
func pairs(fields map[string]string) map[string]*fieldPair {
	out := map[string]*fieldPair{}
	for f, v := range fields {
		base, isN := strings.CutSuffix(f, requestsSuffix)
		p := out[base]
		if p == nil {
			p = &fieldPair{}
			out[base] = p
		}
		if isN {
			p.requests = v
		} else {
			p.spend, p.hasSpend = v, true
		}
	}
	return out
}

// decodeCell turns one pair into a cell; an undecodable field, a non-integer
// value or a requests field without its spend partner (the script always
// writes both, so an orphan is foreign) is an error that names none of them.
func decodeCell(ref hashRef, base string, p *fieldPair) (spendledger.Cell, error) {
	ck, err := decodeField(ref.key, base)
	if err != nil {
		return spendledger.Cell{}, fmt.Errorf("redisledger: hour %s for key %q: a field does not decode: %w", hourName(ref.hour), ref.key, err)
	}
	if !p.hasSpend {
		return spendledger.Cell{}, fmt.Errorf("redisledger: hour %s for key %q: a requests field has no spend field", hourName(ref.hour), ref.key)
	}
	var requests int64
	spend, err := strconv.ParseInt(p.spend, 10, 64)
	if err != nil {
		return spendledger.Cell{}, fmt.Errorf("redisledger: hour %s for key %q: a spend field is not an integer", hourName(ref.hour), ref.key)
	}
	if p.requests != "" {
		if requests, err = strconv.ParseInt(p.requests, 10, 64); err != nil {
			return spendledger.Cell{}, fmt.Errorf("redisledger: hour %s for key %q: a requests field is not an integer", hourName(ref.hour), ref.key)
		}
	}
	return spendledger.Cell{Hour: ref.hour, CellKey: ck, SpendNanoUSD: spend, Requests: requests}, nil
}

// readAll fetches the fields of every ref, chunkSize hashes per pipeline. A
// key under the prefix that is not a hash is foreign (WRONGTYPE): it is
// dropped from the returned refs instead of failing the read.
func (s *Store) readAll(ctx context.Context, refs []hashRef) ([]hashRef, []map[string]string, error) {
	var kept []hashRef
	var out []map[string]string
	for start := 0; start < len(refs); start += chunkSize {
		end := min(start+chunkSize, len(refs))
		pipe := s.client.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, end-start)
		for i, ref := range refs[start:end] {
			cmds[i] = pipe.HGetAll(ctx, ref.name)
		}
		if _, err := pipe.Exec(ctx); err != nil && !isWrongType(err) {
			return nil, nil, fmt.Errorf("redisledger: reading hour hashes %d-%d of %d: %w", start, end, len(refs), err)
		}
		for i, c := range cmds {
			switch err := c.Err(); {
			case err == nil:
				kept = append(kept, refs[start+i])
				out = append(out, c.Val())
			case isWrongType(err):
				continue // foreign: not ours to read or remove
			default:
				return nil, nil, fmt.Errorf("redisledger: reading hour hashes %d-%d of %d: %w", start, end, len(refs), err)
			}
		}
	}
	return kept, out, nil
}

// isWrongType recognises Redis's reply to a command on a key of another type.
func isWrongType(err error) bool { return err != nil && strings.HasPrefix(err.Error(), "WRONGTYPE") }

// deleteAll removes the named keys, chunkSize per DEL.
func (s *Store) deleteAll(ctx context.Context, names []string, what string) error {
	for start := 0; start < len(names); start += chunkSize {
		end := min(start+chunkSize, len(names))
		if err := s.client.Del(ctx, names[start:end]...).Err(); err != nil {
			return fmt.Errorf("redisledger: deleting %s %d-%d of %d: %w", what, start, end, len(names), err)
		}
	}
	return nil
}

// hdel is one HDEL to run: the fields of one hash.
type hdel struct {
	name   string
	fields []string
}

// hdelAll runs the HDELs, chunkSize per pipeline. Redis removes a hash whose
// last field goes.
func (s *Store) hdelAll(ctx context.Context, dels []hdel, what string) error {
	for start := 0; start < len(dels); start += chunkSize {
		end := min(start+chunkSize, len(dels))
		pipe := s.client.Pipeline()
		for _, d := range dels[start:end] {
			pipe.HDel(ctx, d.name, d.fields...)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("redisledger: removing %s from hashes %d-%d of %d: %w", what, start, end, len(dels), err)
		}
	}
	return nil
}

// Query implements spendledger.Ledger: one SCAN of the hour hashes filtered
// by the window, then pipelined HGETALLs; reads fail closed on undecodable
// state.
func (s *Store) Query(ctx context.Context, q spendledger.Query) ([]spendledger.Row, error) {
	if err := spendledger.ValidateGroupBy(q.GroupBy); err != nil {
		return nil, err
	}
	match := keyPrefix + "*"
	if q.Key != "" {
		match = keyPrefix + "*:" + url.QueryEscape(q.Key)
	}
	refs, err := s.scanHashes(ctx, match, q.Key)
	if err != nil {
		return nil, err
	}
	var in []hashRef
	for _, ref := range refs {
		if spendledger.InWindow(ref.hour, q) {
			in = append(in, ref)
		}
	}
	in, fields, err := s.readAll(ctx, in)
	if err != nil {
		return nil, err
	}
	var cells []spendledger.Cell
	for i, ref := range in {
		for base, p := range pairs(fields[i]) {
			c, err := decodeCell(ref, base, p)
			if err != nil {
				return nil, err
			}
			cells = append(cells, c)
		}
	}
	return spendledger.Aggregate(cells, q)
}

// DeleteKey implements spendledger.Ledger: the key's hour hashes and its
// derived state go in chunked DELs (idempotent).
func (s *Store) DeleteKey(ctx context.Context, key string) error {
	refs, err := s.keyHashes(ctx, key)
	if err != nil {
		return err
	}
	names := []string{sessionsKey(key), cellsKey(key)}
	for _, ref := range refs {
		names = append(names, ref.name)
	}
	return s.deleteAll(ctx, names, "the hour hashes and derived state of key "+strconv.Quote(key))
}

// keyState is one key's derived state, computed from hashes a caller has
// just read: the live named sessions and the count of counting cells.
type keyState struct {
	sessions map[string]struct{}
	cells    int
}

// accumulate folds one hash's pairs into st, skipping the bases in exclude
// (the pairs the caller is about to remove) and everything that takes no
// slot: an undecodable field, a pair without a spend field, a sessionless or
// `other` cell.
func accumulate(st *keyState, ref hashRef, ps map[string]*fieldPair, exclude map[string]bool) {
	for base, p := range ps {
		if exclude[base] || !p.hasSpend {
			continue
		}
		ck, err := decodeField(ref.key, base)
		if err != nil || !spendledger.CountsTowardsCaps(ck.Session) {
			continue
		}
		if st.sessions == nil {
			st.sessions = map[string]struct{}{}
		}
		st.sessions[ck.Session] = struct{}{}
		st.cells++
	}
}

// writeStates replaces each key's derived state in one MULTI/EXEC per key:
// both keys deleted, then the set and the count written when the key has
// counting cells. The window between the caller's read and this write is the
// package doc's "eventually exact".
func (s *Store) writeStates(ctx context.Context, states map[string]*keyState) error {
	for key, st := range states {
		pipe := s.client.TxPipeline()
		pipe.Del(ctx, sessionsKey(key), cellsKey(key))
		if st.cells > 0 {
			members := make([]interface{}, 0, len(st.sessions))
			for sess := range st.sessions {
				members = append(members, sess)
			}
			pipe.SAdd(ctx, sessionsKey(key), members...)
			pipe.Set(ctx, cellsKey(key), st.cells, 0)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("redisledger: rebuilding the derived state of key %q: %w", key, err)
		}
	}
	return nil
}

// DeleteSession implements spendledger.Ledger: the session's field pairs go
// from every hour hash of every key, then the touched keys' derived state is
// rebuilt from the hashes just read. It reads every hour hash in the store
// -- hours x keys HGETALLs, in chunks -- which is the price of erasing across
// keys; an admin operation, not a hot path. Fields that do not decode are
// left for Sweep.
func (s *Store) DeleteSession(ctx context.Context, session string) (int, error) {
	if session == "" || session == spendledger.OtherSession {
		return 0, spendledger.ErrSessionNotErasable
	}
	if !spendledger.IsSessionIdentifier(session) {
		return 0, spendledger.ErrSessionNotIdentifier
	}
	refs, err := s.scanHashes(ctx, keyPrefix+"*", "")
	if err != nil {
		return 0, err
	}
	refs, fields, err := s.readAll(ctx, refs)
	if err != nil {
		return 0, err
	}
	n := 0
	var dels []hdel
	excluded := make([]map[string]bool, len(refs))
	touched := map[string]*keyState{}
	for i, ref := range refs {
		excluded[i] = map[string]bool{}
		for base, p := range pairs(fields[i]) {
			ck, err := decodeField(ref.key, base)
			if err != nil || ck.Session != session {
				continue
			}
			excluded[i][base] = true
			if p.hasSpend {
				n++
			}
		}
		if len(excluded[i]) > 0 {
			d := hdel{name: ref.name}
			for base := range excluded[i] {
				d.fields = append(d.fields, base, base+requestsSuffix)
			}
			dels = append(dels, d)
			touched[ref.key] = &keyState{}
		}
	}
	for i, ref := range refs {
		if st := touched[ref.key]; st != nil {
			accumulate(st, ref, pairs(fields[i]), excluded[i])
		}
	}
	if err := s.hdelAll(ctx, dels, "a session"); err != nil {
		return 0, err
	}
	return n, s.writeStates(ctx, touched)
}

// Sweep implements spendledger.Ledger: hour hashes whose hour starts before
// HourBucket(now) - Retention go (the hash at the cutoff stays); in the
// hashes that stay, field pairs that do not decode or carry a non-integer
// value are removed, so reads recover; then every key's derived state is
// rebuilt from the hashes just read -- including keys whose hashes are all
// gone, whose state is removed.
func (s *Store) Sweep(ctx context.Context, now time.Time) error {
	cutoff := spendledger.HourBucket(now).Add(-s.opts.Retention)
	refs, err := s.scanHashes(ctx, keyPrefix+"*", "")
	if err != nil {
		return err
	}
	states := map[string]*keyState{}
	var doomed []string
	var live []hashRef
	for _, ref := range refs {
		states[ref.key] = &keyState{}
		if ref.hour.Before(cutoff) {
			doomed = append(doomed, ref.name)
		} else {
			live = append(live, ref)
		}
	}
	if err := s.deleteAll(ctx, doomed, "expired hour hashes"); err != nil {
		return err
	}
	live, fields, err := s.readAll(ctx, live)
	if err != nil {
		return err
	}
	var garbageDels []hdel
	for i, ref := range live {
		ps := pairs(fields[i])
		garbage := map[string]bool{}
		for base, p := range ps {
			if _, err := decodeCell(ref, base, p); err != nil {
				garbage[base] = true
			}
		}
		if len(garbage) > 0 {
			d := hdel{name: ref.name}
			for base := range garbage {
				d.fields = append(d.fields, base, base+requestsSuffix)
			}
			garbageDels = append(garbageDels, d)
		}
		accumulate(states[ref.key], ref, ps, garbage)
	}
	if err := s.hdelAll(ctx, garbageDels, "undecodable fields"); err != nil {
		return err
	}
	for _, segment := range []string{sessionsSegment, cellsSegment} {
		if err := s.derivedKeys(ctx, segment, states); err != nil {
			return err
		}
	}
	return s.writeStates(ctx, states)
}

// derivedKeys adds, with an empty state, every key that owns derived state
// under segment and is not already in states: its hashes are all gone, so
// its state must go too.
func (s *Store) derivedKeys(ctx context.Context, segment string, states map[string]*keyState) error {
	prefix := keyPrefix + segment + ":"
	iter := s.client.Scan(ctx, 0, prefix+"*", scanCount).Iterator()
	for iter.Next(ctx) {
		escKey, found := strings.CutPrefix(iter.Val(), prefix)
		if !found || strings.Contains(escKey, ":") {
			continue
		}
		key, err := url.QueryUnescape(escKey)
		if err != nil {
			continue
		}
		if _, known := states[key]; !known {
			states[key] = &keyState{}
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("redisledger: scanning %s state: %w", segment, err)
	}
	return nil
}
