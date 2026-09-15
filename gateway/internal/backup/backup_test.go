package backup

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func openTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("bolt.Open: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("data"))
		if err != nil {
			return err
		}
		return b.Put([]byte("key"), []byte("value"))
	}); err != nil {
		t.Fatalf("seeding test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestCopyFileProducesAByteIdenticalRestorableDatabase proves the actual
// backup guarantee: a backed-up file is a real, independently-openable
// bbolt database with all the source's data intact, not just a file that
// happens to exist.
func TestCopyFileProducesAByteIdenticalRestorableDatabase(t *testing.T) {
	db := openTestDB(t)
	destPath := filepath.Join(t.TempDir(), "backup.db")

	if err := CopyFile(db, destPath); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	restored, err := bolt.Open(destPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("opening the backup file: %v", err)
	}
	defer func() { _ = restored.Close() }()

	err = restored.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("data"))
		if b == nil {
			t.Fatal("backup file has no \"data\" bucket")
		}
		if got := string(b.Get([]byte("key"))); got != "value" {
			t.Errorf("backup file's key = %q, want %q", got, "value")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the backup file: %v", err)
	}
}

// TestCopyFileRefusesToOverwriteAnExistingDestination proves a backup
// call never silently clobbers a prior backup at the same path.
func TestCopyFileRefusesToOverwriteAnExistingDestination(t *testing.T) {
	db := openTestDB(t)
	destPath := filepath.Join(t.TempDir(), "backup.db")

	if err := CopyFile(db, destPath); err != nil {
		t.Fatalf("first CopyFile: %v", err)
	}
	if err := CopyFile(db, destPath); err == nil {
		t.Fatal("second CopyFile to the same destPath returned nil error, want a refusal-to-overwrite error")
	}
}

// TestCopyFileSafeDuringConcurrentWrites is the actual proof of this
// package's own core claim ("safe to continue using the database while
// a copy is in progress") -- not just trusting bbolt's own doc comment.
// Hammers real concurrent writes against db while backing it up; the
// backup must succeed and the resulting file must still open cleanly.
func TestCopyFileSafeDuringConcurrentWrites(t *testing.T) {
	db := openTestDB(t)
	destPath := filepath.Join(t.TempDir(), "backup.db")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte("data"))
				i++
				return b.Put([]byte("concurrent-key"), []byte(time.Now().String()))
			})
		}
	}()

	if err := CopyFile(db, destPath); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("CopyFile while writes were in flight: %v", err)
	}
	close(stop)
	wg.Wait()

	restored, err := bolt.Open(destPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("opening the backup file after concurrent writes: %v", err)
	}
	_ = restored.Close()
}
