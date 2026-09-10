package prompt

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

func msgs(content string) []adapter.Message {
	return []adapter.Message{{Role: "system", Content: content}}
}

func TestUpsertAutoBumpsVersion(t *testing.T) {
	s := NewStore()

	v1, err := s.Upsert("greeting", msgs("hello v1"))
	if err != nil {
		t.Fatalf("Upsert v1: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("first Upsert: Version = %d, want 1", v1.Version)
	}

	v2, err := s.Upsert("greeting", msgs("hello v2"))
	if err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("second Upsert: Version = %d, want 2", v2.Version)
	}

	v3, err := s.Upsert("greeting", msgs("hello v3"))
	if err != nil {
		t.Fatalf("Upsert v3: %v", err)
	}
	if v3.Version != 3 {
		t.Fatalf("third Upsert: Version = %d, want 3", v3.Version)
	}
}

func TestUpsertRejectsEmptyID(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("", msgs("x")); err == nil {
		t.Fatalf("Upsert with empty id: got nil error, want a real error")
	}
}

// TestPinnedHistoricalVersionStaysStableAfterALaterEditCreatesANewVersion
// is the load-bearing proof for this feature's whole "append-only version
// history" premise: a caller that pinned version 1 by ID+number must see
// its exact original content forever, even after version 2/3/... exist.
func TestPinnedHistoricalVersionStaysStableAfterALaterEditCreatesANewVersion(t *testing.T) {
	s := NewStore()

	v1, err := s.Upsert("greeting", msgs("hello v1"))
	if err != nil {
		t.Fatalf("Upsert v1: %v", err)
	}

	if _, err := s.Upsert("greeting", msgs("hello v2 -- totally different")); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}
	if _, err := s.Upsert("greeting", msgs("hello v3 -- different again")); err != nil {
		t.Fatalf("Upsert v3: %v", err)
	}

	pinned, ok := s.Get("greeting", v1.Version)
	if !ok {
		t.Fatalf("Get(greeting, 1): not found")
	}
	if pinned.Messages[0].Content != "hello v1" {
		t.Errorf("pinned version 1 content = %q, want %q (must stay stable after later versions were created)", pinned.Messages[0].Content, "hello v1")
	}

	latest, ok := s.Get("greeting", 0)
	if !ok {
		t.Fatalf("Get(greeting, latest): not found")
	}
	if latest.Version != 3 || latest.Messages[0].Content != "hello v3 -- different again" {
		t.Errorf("latest = %+v, want version 3 with the v3 content", latest)
	}
}

func TestGetUnknownIDOrVersion(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("hello")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, ok := s.Get("does-not-exist", 0); ok {
		t.Errorf("Get with unknown id: ok = true, want false")
	}
	if _, ok := s.Get("greeting", 99); ok {
		t.Errorf("Get with unknown version: ok = true, want false")
	}
}

func TestListReturnsLatestVersionOfEveryPromptSortedByID(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("zeta", msgs("z1")); err != nil {
		t.Fatalf("Upsert zeta: %v", err)
	}
	if _, err := s.Upsert("alpha", msgs("a1")); err != nil {
		t.Fatalf("Upsert alpha: %v", err)
	}
	if _, err := s.Upsert("alpha", msgs("a2")); err != nil {
		t.Fatalf("Upsert alpha v2: %v", err)
	}

	list := s.List()
	if len(list) != 2 {
		t.Fatalf("List() returned %d entries, want 2", len(list))
	}
	if list[0].ID != "alpha" || list[1].ID != "zeta" {
		t.Errorf("List() order = [%s, %s], want [alpha, zeta] (sorted by ID)", list[0].ID, list[1].ID)
	}
	if list[0].Version != 2 {
		t.Errorf("List()'s alpha entry has Version %d, want 2 (the latest)", list[0].Version)
	}
}

func TestDelete(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("hello")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.Delete("greeting"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get("greeting", 0); ok {
		t.Errorf("Get after Delete: ok = true, want false")
	}
	if _, _, err := s.Resolve("greeting", 0, nil); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Resolve after Delete: err = %v, want ErrPromptNotFound", err)
	}
}

func TestDeleteUnknownIDReturnsErrPromptNotFound(t *testing.T) {
	s := NewStore()
	if err := s.Delete("does-not-exist"); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Delete(unknown): err = %v, want ErrPromptNotFound", err)
	}
}

func TestResolveSubstitutesKnownVariables(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", []adapter.Message{
		{Role: "system", Content: "You are {{persona}}."},
		{Role: "user", Content: "Hello, {{name}}! Today is {{day}}."},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	resolved, _, err := s.Resolve("greeting", 0, map[string]string{"persona": "a pirate", "name": "Ada"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved[0].Content != "You are a pirate." {
		t.Errorf("resolved[0].Content = %q, want %q", resolved[0].Content, "You are a pirate.")
	}
	// {{day}} has no matching key in variables -- per this package's own
	// documented design decision, it must be left as literal,
	// UNCHANGED text, never an error and never silently dropped.
	want := "Hello, Ada! Today is {{day}}."
	if resolved[1].Content != want {
		t.Errorf("resolved[1].Content = %q, want %q (unresolved placeholder left as literal text)", resolved[1].Content, want)
	}
}

func TestResolveSubstitutesTextContentPartsNotJustMessageContent(t *testing.T) {
	// A multi-modal message's own lead-in/trailing text lives in
	// Parts[i].Text (Type == "text"), independently of Content -- see
	// adapter.Message's own doc comment. Substitution must reach a
	// text part too, or a template combining an image part with a
	// "{{name}}"-bearing text part would silently skip that part.
	s := NewStore()
	if _, err := s.Upsert("multimodal", []adapter.Message{
		{
			Role:    "user",
			Content: "Hello, {{name}}!",
			Parts: []adapter.ContentPart{
				{Type: "text", Text: "Caption for {{name}}'s photo:"},
				{Type: "image", MediaType: "image/png", Data: "base64data"},
			},
		},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	resolved, _, err := s.Resolve("multimodal", 0, map[string]string{"name": "Ada"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved[0].Content != "Hello, Ada!" {
		t.Errorf("resolved[0].Content = %q, want %q", resolved[0].Content, "Hello, Ada!")
	}
	if resolved[0].Parts[0].Text != "Caption for Ada's photo:" {
		t.Errorf("resolved[0].Parts[0].Text = %q, want %q", resolved[0].Parts[0].Text, "Caption for Ada's photo:")
	}
	// The non-text (image) part must be passed through byte-identical --
	// substitution must never touch MediaType/Data/URL.
	if resolved[0].Parts[1].MediaType != "image/png" || resolved[0].Parts[1].Data != "base64data" {
		t.Errorf("resolved[0].Parts[1] was mutated: got %+v", resolved[0].Parts[1])
	}
}

func TestResolveWithNoVariablesLeavesEveryPlaceholderLiteral(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("Hello, {{name}}!")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	resolved, _, err := s.Resolve("greeting", 0, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved[0].Content != "Hello, {{name}}!" {
		t.Errorf("resolved[0].Content = %q, want %q", resolved[0].Content, "Hello, {{name}}!")
	}
}

func TestResolveUnknownPromptReturnsErrPromptNotFound(t *testing.T) {
	s := NewStore()
	if _, _, err := s.Resolve("does-not-exist", 0, nil); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Resolve(unknown id): err = %v, want ErrPromptNotFound", err)
	}

	if _, err := s.Upsert("greeting", msgs("hi")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, _, err := s.Resolve("greeting", 99, nil); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Resolve(unknown version): err = %v, want ErrPromptNotFound", err)
	}
}

// TestResolveFingerprintIsStableForRepeatedCallsAgainstTheSameVersion and
// TestResolveFingerprintChangesWhenTheUnderlyingContentChanges are the
// load-bearing proof for Resolve's own cache-key-fold contract.
func TestResolveFingerprintIsStableForRepeatedCallsAgainstTheSameVersion(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("hello")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	_, fp1, err := s.Resolve("greeting", 0, map[string]string{"unused": "x"})
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	_, fp2, err := s.Resolve("greeting", 0, nil)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint differed across repeated calls against the same version: %q != %q", fp1, fp2)
	}
}

func TestResolveFingerprintChangesWhenTheUnderlyingContentChanges(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("hello v1")); err != nil {
		t.Fatalf("Upsert v1: %v", err)
	}
	_, fpV1, err := s.Resolve("greeting", 0, nil)
	if err != nil {
		t.Fatalf("Resolve v1: %v", err)
	}

	if _, err := s.Upsert("greeting", msgs("hello v2")); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}
	_, fpV2, err := s.Resolve("greeting", 0, nil)
	if err != nil {
		t.Fatalf("Resolve v2: %v", err)
	}

	if fpV1 == fpV2 {
		t.Errorf("fingerprint did not change when the underlying prompt content changed (new version): %q == %q", fpV1, fpV2)
	}

	// The OLD, pinned version's own fingerprint must still reproduce
	// exactly what it always did -- fingerprintFor is derived fresh from
	// Get's result, so this is automatic, but worth asserting directly.
	_, fpV1Again, err := s.Resolve("greeting", 1, nil)
	if err != nil {
		t.Fatalf("Resolve pinned v1: %v", err)
	}
	if fpV1Again != fpV1 {
		t.Errorf("pinned version 1's fingerprint changed after v2 was created: %q != %q", fpV1Again, fpV1)
	}
}

// TestConcurrentUpsertGetResolveUnderRace hammers Upsert/Get/Resolve from
// many goroutines simultaneously against a handful of shared IDs, run
// under -race (per go test -race). Confirms two things Store's own
// CompareAndSwap-retry design (see its doc comment) exists specifically
// to guarantee: no data race (the race detector itself would fail this
// test), and no lost/corrupted write -- every one of the
// writersPerID*ids.length Upsert calls per ID must be reflected somewhere
// in that ID's final version count, never silently dropped by a
// concurrent CAS loser that gave up instead of retrying.
func TestConcurrentUpsertGetResolveUnderRace(t *testing.T) {
	s := NewStore()
	ids := []string{"alpha", "beta", "gamma"}
	const writersPerID = 50
	const readersPerID = 50

	var wg sync.WaitGroup
	for _, id := range ids {
		for i := 0; i < writersPerID; i++ {
			wg.Add(1)
			go func(id string, i int) {
				defer wg.Done()
				if _, err := s.Upsert(id, msgs(fmt.Sprintf("content-%d", i))); err != nil {
					t.Errorf("Upsert(%q, #%d): %v", id, i, err)
				}
			}(id, i)
		}
		for i := 0; i < readersPerID; i++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				s.Get(id, 0)
				_, _, _ = s.Resolve(id, 0, map[string]string{"x": "y"})
				s.List()
			}(id)
		}
	}
	wg.Wait()

	for _, id := range ids {
		latest, ok := s.Get(id, 0)
		if !ok {
			t.Fatalf("Get(%q, latest) after concurrent Upserts: not found", id)
		}
		if latest.Version != writersPerID {
			t.Errorf("Get(%q, latest).Version = %d, want %d (every concurrent Upsert must be reflected -- a lost write would undercount this)", id, latest.Version, writersPerID)
		}
		// Every version from 1..writersPerID must be independently
		// retrievable -- a corrupted write (e.g. two different Upserts
		// both landing on the same version number) would show up here as
		// a missing or duplicated version.
		seen := map[int]bool{}
		for v := 1; v <= writersPerID; v++ {
			p, ok := s.Get(id, v)
			if !ok {
				t.Errorf("Get(%q, %d): not found -- a version was lost under concurrent writes", id, v)
				continue
			}
			if p.Version != v {
				t.Errorf("Get(%q, %d).Version = %d, want %d", id, v, p.Version, v)
			}
			seen[v] = true
		}
		if len(seen) != writersPerID {
			t.Errorf("id %q: saw %d distinct versions, want %d", id, len(seen), writersPerID)
		}
	}
}

func TestNewStoreWithPersisterLoadsExistingPrompts(t *testing.T) {
	fp := &fakePersister{
		data: map[string][]Prompt{
			"greeting": {{ID: "greeting", Version: 1, Messages: msgs("preloaded")}},
		},
	}
	s, err := NewStoreWithPersister(context.Background(), fp)
	if err != nil {
		t.Fatalf("NewStoreWithPersister: %v", err)
	}
	got, ok := s.Get("greeting", 0)
	if !ok {
		t.Fatalf("Get after NewStoreWithPersister: not found")
	}
	if got.Messages[0].Content != "preloaded" {
		t.Errorf("got.Messages[0].Content = %q, want %q", got.Messages[0].Content, "preloaded")
	}
}

func TestNewStoreWithPersisterPropagatesLoadError(t *testing.T) {
	fp := &fakePersister{loadErr: errors.New("boom")}
	if _, err := NewStoreWithPersister(context.Background(), fp); err == nil {
		t.Fatalf("NewStoreWithPersister: got nil error, want the Load failure surfaced")
	}
}

func TestUpsertAndDeletePersistThroughPersister(t *testing.T) {
	fp := &fakePersister{data: map[string][]Prompt{}}
	s, err := NewStoreWithPersister(context.Background(), fp)
	if err != nil {
		t.Fatalf("NewStoreWithPersister: %v", err)
	}

	if _, err := s.Upsert("greeting", msgs("hi")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	fp.mu.Lock()
	saved := len(fp.saved["greeting"])
	fp.mu.Unlock()
	if saved != 1 {
		t.Errorf("persister recorded %d versions after Upsert, want 1", saved)
	}

	if err := s.Delete("greeting"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	fp.mu.Lock()
	deletedVersions, ok := fp.saved["greeting"]
	fp.mu.Unlock()
	if !ok || len(deletedVersions) != 0 {
		t.Errorf("persister's saved state for greeting after Delete = %v, want an empty/nil slice", deletedVersions)
	}
}

// fakePersister is a minimal in-memory Persister for tests -- production
// code has no real implementation of this interface yet (see Persister's
// own doc comment).
type fakePersister struct {
	mu      sync.Mutex
	data    map[string][]Prompt
	saved   map[string][]Prompt
	loadErr error
}

func (f *fakePersister) Load(ctx context.Context) (map[string][]Prompt, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.data, nil
}

func (f *fakePersister) Save(ctx context.Context, id string, versions []Prompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saved == nil {
		f.saved = map[string][]Prompt{}
	}
	f.saved[id] = versions
	return nil
}
