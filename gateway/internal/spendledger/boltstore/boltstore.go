// Package boltstore is the bbolt backend of the spend ledger (item 13d):
// one file (the one `ledger.persist_path` will name once slice 3 wires the
// config), a top-level bucket per UTC hour so retention is one bucket
// delete, and the derived per-key session set and row count kept beside the
// rows so the caps hold across a restart. OpenWith returns the opener shape
// cmd/gateway's openPersistStoreWithRecovery takes, like the budget,
// identity and prompt stores.
package boltstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
)

const (
	hoursBucket    = "hours"    // nested: one bucket per hour, named by its Unix seconds
	sessionsBucket = "sessions" // nested: one bucket per key, members are session names
	rowsBucket     = "rows"     // key -> count of distinct live rows that take a slot, as decimal text
	// openLockTimeout bounds how long Open waits for bbolt's exclusive file
	// lock, like the other stores: a second process holding the file fails
	// the start within a second instead of hanging it.
	openLockTimeout = time.Second
)

// Store is a bbolt-backed spendledger.Ledger.
type Store struct {
	db   *bolt.DB
	opts spendledger.Options
}

type sums struct {
	Spend    int64 `json:"spend"`
	Requests int64 `json:"requests"`
}

// Open opens path with the default options.
func Open(path string) (*Store, error) { return OpenWith(spendledger.Options{})(path) }

// OpenWith returns an opener bound to opts, the shape
// openPersistStoreWithRecovery takes.
func OpenWith(opts spendledger.Options) func(path string) (*Store, error) {
	opts = opts.WithDefaults()
	return func(path string) (*Store, error) {
		db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: openLockTimeout})
		if err != nil {
			if errors.Is(err, berrors.ErrTimeout) {
				return nil, fmt.Errorf("spendledger/boltstore: opening %s: another process holds the file lock (waited %s): %w", path, openLockTimeout, err)
			}
			return nil, fmt.Errorf("spendledger/boltstore: opening %s: %w", path, err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			for _, name := range []string{hoursBucket, sessionsBucket, rowsBucket} {
				if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("spendledger/boltstore: creating buckets: %w", err)
		}
		return &Store{db: db, opts: opts}, nil
	}
}

// Close implements spendledger.Ledger.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for backup.CopyFile (backup.DBBacked).
func (s *Store) DB() *bolt.DB { return s.db }

func hourName(hour time.Time) []byte { return []byte(strconv.FormatInt(hour.Unix(), 10)) }

// parseHour reads an hour bucket's name; the name is quoted in the error
// because it is a number of the store's own making, never caller data.
func parseHour(name []byte) (time.Time, error) {
	n, err := strconv.ParseInt(string(name), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("spendledger/boltstore: hour bucket %q is not a Unix timestamp: %w", name, err)
	}
	return time.Unix(n, 0).UTC(), nil
}

// rowCount reads a key's count of live rows that take a slot (0 when the key
// has none). A value that is not a decimal count is an error, never zero:
// zero would lift the cap.
func rowCount(rows *bolt.Bucket, key string) (int, error) {
	v := rows.Get([]byte(key))
	if v == nil {
		return 0, nil
	}
	n, err := strconv.Atoi(string(v))
	if err != nil {
		return 0, fmt.Errorf("spendledger/boltstore: row count for key %q is corrupt: %w", key, err)
	}
	return n, nil
}

// Record implements spendledger.Ledger.
func (s *Store) Record(_ context.Context, e spendledger.Entry) error {
	if err := spendledger.ValidateEntry(e); err != nil {
		return err
	}
	hour := spendledger.HourBucket(e.At)
	return s.db.Update(func(tx *bolt.Tx) error {
		sessions, err := tx.Bucket([]byte(sessionsBucket)).CreateBucketIfNotExists([]byte(e.Key))
		if err != nil {
			return err
		}
		live := map[string]struct{}{}
		if err := sessions.ForEach(func(k, _ []byte) error { live[string(k)] = struct{}{}; return nil }); err != nil {
			return err
		}
		ck := spendledger.CellKey{Key: e.Key, Model: e.Model, Tool: e.Tool, Session: spendledger.FoldSession(e.Session, live, s.opts.MaxSessionsPerKey)}
		hours, err := tx.Bucket([]byte(hoursBucket)).CreateBucketIfNotExists(hourName(hour))
		if err != nil {
			return err
		}
		rows := tx.Bucket([]byte(rowsBucket))
		counts := spendledger.CountsTowardsCaps(ck.Session)
		existing := hours.Get([]byte(ck.Encode()))
		n, err := rowCount(rows, e.Key)
		if err != nil {
			return err
		}
		if existing == nil && counts && n >= s.opts.MaxRowsPerKey {
			ck.Session = spendledger.OtherSession
			counts = false
			existing = hours.Get([]byte(ck.Encode()))
		}
		var cur sums
		if existing != nil {
			if err := json.Unmarshal(existing, &cur); err != nil {
				return fmt.Errorf("spendledger/boltstore: hour %s: a row for key %q has a corrupt value: %w", hourName(hour), e.Key, err)
			}
		} else if counts {
			if err := rows.Put([]byte(e.Key), []byte(strconv.Itoa(n+1))); err != nil {
				return err
			}
		}
		cur.Spend += e.SpendNanoUSD
		cur.Requests += e.Requests
		encoded, err := json.Marshal(cur)
		if err != nil {
			return err
		}
		if err := hours.Put([]byte(ck.Encode()), encoded); err != nil {
			return err
		}
		if counts {
			return sessions.Put([]byte(ck.Session), []byte{})
		}
		return nil
	})
}

// cells reads every live row; the caller filters and aggregates.
// cellsIn reads the cells of the hour buckets q selects. Reads fail closed on
// undecodable state (an hour name that is not a Unix timestamp, a row key that
// does not decode, a value that is not JSON) and the errors name the hour and
// the position, never the row: a cell key may carry a session identifier and
// these errors reach log lines. Sweep removes such state.
func (s *Store) cellsIn(tx *bolt.Tx, q spendledger.Query) ([]spendledger.Cell, error) {
	var out []spendledger.Cell
	hours := tx.Bucket([]byte(hoursBucket))
	err := hours.ForEachBucket(func(name []byte) error {
		hour, err := parseHour(name)
		if err != nil {
			return err
		}
		if !spendledger.InWindow(hour, q) {
			return nil
		}
		i := 0
		return hours.Bucket(name).ForEach(func(k, v []byte) error {
			i++
			ck, err := spendledger.DecodeCellKey(string(k))
			if err != nil {
				return fmt.Errorf("spendledger/boltstore: hour %s row %d: %w", name, i, err)
			}
			var cur sums
			if err := json.Unmarshal(v, &cur); err != nil {
				return fmt.Errorf("spendledger/boltstore: hour %s row %d for key %q has a corrupt value: %w", name, i, ck.Key, err)
			}
			out = append(out, spendledger.Cell{Hour: hour, CellKey: ck, SpendNanoUSD: cur.Spend, Requests: cur.Requests})
			return nil
		})
	})
	return out, err
}

// Query implements spendledger.Ledger.
func (s *Store) Query(_ context.Context, q spendledger.Query) ([]spendledger.Row, error) {
	if err := spendledger.ValidateGroupBy(q.GroupBy); err != nil {
		return nil, err
	}
	var cells []spendledger.Cell
	if err := s.db.View(func(tx *bolt.Tx) (err error) {
		cells, err = s.cellsIn(tx, q)
		return err
	}); err != nil {
		return nil, err
	}
	return spendledger.Aggregate(cells, q)
}

// deleteWhere removes every row for which match returns true and returns
// the count, drops any hour bucket that is left empty, then rebuilds the
// derived state for the keys touched.
func (s *Store) deleteWhere(match func(spendledger.CellKey) bool) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		hours := tx.Bucket([]byte(hoursBucket))
		var emptied [][]byte
		touched := map[string]struct{}{}
		if err := hours.ForEachBucket(func(name []byte) error {
			b := hours.Bucket(name)
			var doomed [][]byte
			total := 0
			var garbage [][]byte
			if err := b.ForEach(func(k, _ []byte) error {
				total++
				ck, err := spendledger.DecodeCellKey(string(k))
				if err != nil {
					garbage = append(garbage, append([]byte(nil), k...)) // undecodable: removed, not counted
					return nil
				}
				if match(ck) {
					doomed = append(doomed, append([]byte(nil), k...))
					touched[ck.Key] = struct{}{}
				}
				return nil
			}); err != nil {
				return err
			}
			for _, k := range doomed {
				if err := b.Delete(k); err != nil {
					return err
				}
				n++
			}
			for _, k := range garbage {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
			// Bucket.Stats walks committed pages, not this transaction's
			// deletes, so emptiness is the count we just took.
			if len(doomed)+len(garbage) == total {
				emptied = append(emptied, append([]byte(nil), name...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, name := range emptied {
			if err := hours.DeleteBucket(name); err != nil {
				return err
			}
		}
		return s.rebuild(tx, touched)
	})
	return n, err
}

// rebuild recomputes the per-key session set and row count from the live
// hours, for the given keys (nil: every key). A row whose key does not decode
// or whose value is not the store's JSON is garbage the store cannot read: it
// is removed, so that reads recover and a rebuild -- and with it retention and
// erasure -- never stalls on it.
func (s *Store) rebuild(tx *bolt.Tx, keys map[string]struct{}) error {
	hours := tx.Bucket([]byte(hoursBucket))
	sessions := tx.Bucket([]byte(sessionsBucket))
	rows := tx.Bucket([]byte(rowsBucket))
	liveSessions := map[string]map[string]struct{}{}
	liveRows := map[string]int{}
	if err := hours.ForEachBucket(func(name []byte) error {
		b := hours.Bucket(name)
		var garbage [][]byte
		if err := b.ForEach(func(k, v []byte) error {
			ck, err := spendledger.DecodeCellKey(string(k))
			if err != nil || json.Unmarshal(v, &sums{}) != nil {
				garbage = append(garbage, append([]byte(nil), k...)) // undecodable: removed
				return nil
			}
			if keys != nil {
				if _, ok := keys[ck.Key]; !ok {
					return nil
				}
			}
			if !spendledger.CountsTowardsCaps(ck.Session) {
				return nil // takes no slot: not counted, not a live session
			}
			liveRows[ck.Key]++
			if liveSessions[ck.Key] == nil {
				liveSessions[ck.Key] = map[string]struct{}{}
			}
			liveSessions[ck.Key][ck.Session] = struct{}{}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range garbage {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var scope []string
	if keys == nil {
		if err := sessions.ForEachBucket(func(name []byte) error { scope = append(scope, string(name)); return nil }); err != nil {
			return err
		}
		if err := rows.ForEach(func(k, _ []byte) error { scope = append(scope, string(k)); return nil }); err != nil {
			return err
		}
		for k := range liveRows {
			scope = append(scope, k)
		}
	} else {
		for k := range keys {
			scope = append(scope, k)
		}
	}
	done := map[string]bool{}
	for _, key := range scope {
		if done[key] {
			continue
		}
		done[key] = true
		if sessions.Bucket([]byte(key)) != nil {
			if err := sessions.DeleteBucket([]byte(key)); err != nil {
				return err
			}
		}
		if err := rows.Delete([]byte(key)); err != nil {
			return err
		}
		if liveRows[key] == 0 {
			continue
		}
		if err := rows.Put([]byte(key), []byte(strconv.Itoa(liveRows[key]))); err != nil {
			return err
		}
		b, err := sessions.CreateBucketIfNotExists([]byte(key))
		if err != nil {
			return err
		}
		for sess := range liveSessions[key] {
			if err := b.Put([]byte(sess), []byte{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteKey implements spendledger.Ledger.
func (s *Store) DeleteKey(_ context.Context, key string) error {
	_, err := s.deleteWhere(func(ck spendledger.CellKey) bool { return ck.Key == key })
	return err
}

// DeleteSession implements spendledger.Ledger.
func (s *Store) DeleteSession(_ context.Context, session string) (int, error) {
	if session == "" || session == spendledger.OtherSession {
		return 0, spendledger.ErrSessionNotErasable
	}
	if !spendledger.IsSessionIdentifier(session) {
		return 0, spendledger.ErrSessionNotIdentifier
	}
	return s.deleteWhere(func(ck spendledger.CellKey) bool { return ck.Session == session })
}

// Sweep implements spendledger.Ledger: whole hour buckets that start before
// HourBucket(now) - Retention go (the bucket at the cutoff stays), as does any
// bucket whose name is not a timestamp (it cannot be dated, so it cannot be
// kept); then every key's derived state is rebuilt, dropping undecodable rows.
func (s *Store) Sweep(_ context.Context, now time.Time) error {
	cutoff := spendledger.HourBucket(now).Add(-s.opts.Retention)
	return s.db.Update(func(tx *bolt.Tx) error {
		hours := tx.Bucket([]byte(hoursBucket))
		var doomed [][]byte
		if err := hours.ForEachBucket(func(name []byte) error {
			hour, err := parseHour(name)
			if err != nil || hour.Before(cutoff) {
				// undated or expired: either way the bucket cannot be kept
				doomed = append(doomed, append([]byte(nil), name...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, name := range doomed {
			if err := hours.DeleteBucket(name); err != nil {
				return err
			}
		}
		return s.rebuild(tx, nil)
	})
}
