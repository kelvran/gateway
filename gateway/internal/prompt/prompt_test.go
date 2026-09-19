package prompt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	if _, _, _, err := s.Resolve("greeting", 0, nil); !errors.Is(err, ErrPromptNotFound) {
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

	resolved, _, _, err := s.Resolve("greeting", 0, map[string]string{"persona": "a pirate", "name": "Ada"})
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

// TestResolveDoesNotRecursivelyExpandPlaceholderShapedVariableValues is
// the regression proof for a real gap: substitute()'s own
// ReplaceAllStringFunc call scans the ORIGINAL content exactly once --
// the replacement text a variable resolves to is never itself re-scanned
// for further {{...}} placeholders. Never previously exercised with a
// variable VALUE that happens to look like another placeholder.
func TestResolveDoesNotRecursivelyExpandPlaceholderShapedVariableValues(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", []adapter.Message{
		{Role: "user", Content: "Hello, {{name}}!"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	resolved, _, _, err := s.Resolve("greeting", 0, map[string]string{"name": "{{secret}}", "secret": "LEAKED"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := "Hello, {{secret}}!"
	if resolved[0].Content != want {
		t.Errorf("resolved[0].Content = %q, want %q (the literal replacement text, never re-scanned into a second substitution pass)", resolved[0].Content, want)
	}
}

// TestResolveTerminatesOnASelfReferentialVariableValue is the same
// proof's termination half: a variable whose OWN value is its own
// placeholder text must not cause an infinite substitution loop --
// impossible with substitute()'s actual single-pass implementation, but
// worth pinning down explicitly rather than only inferring it from the
// non-recursion test above.
func TestResolveTerminatesOnASelfReferentialVariableValue(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("self-ref", []adapter.Message{
		{Role: "user", Content: "Hi {{name}}"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	resolved, _, _, err := s.Resolve("self-ref", 0, map[string]string{"name": "{{name}}"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := "Hi {{name}}"
	if resolved[0].Content != want {
		t.Errorf("resolved[0].Content = %q, want %q", resolved[0].Content, want)
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

	resolved, _, _, err := s.Resolve("multimodal", 0, map[string]string{"name": "Ada"})
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

	resolved, _, _, err := s.Resolve("greeting", 0, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved[0].Content != "Hello, {{name}}!" {
		t.Errorf("resolved[0].Content = %q, want %q", resolved[0].Content, "Hello, {{name}}!")
	}
}

func TestResolveUnknownPromptReturnsErrPromptNotFound(t *testing.T) {
	s := NewStore()
	if _, _, _, err := s.Resolve("does-not-exist", 0, nil); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Resolve(unknown id): err = %v, want ErrPromptNotFound", err)
	}

	if _, err := s.Upsert("greeting", msgs("hi")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, _, _, err := s.Resolve("greeting", 99, nil); !errors.Is(err, ErrPromptNotFound) {
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

	_, fp1, _, err := s.Resolve("greeting", 0, map[string]string{"unused": "x"})
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	_, fp2, _, err := s.Resolve("greeting", 0, nil)
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
	_, fpV1, _, err := s.Resolve("greeting", 0, nil)
	if err != nil {
		t.Fatalf("Resolve v1: %v", err)
	}

	if _, err := s.Upsert("greeting", msgs("hello v2")); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}
	_, fpV2, _, err := s.Resolve("greeting", 0, nil)
	if err != nil {
		t.Fatalf("Resolve v2: %v", err)
	}

	if fpV1 == fpV2 {
		t.Errorf("fingerprint did not change when the underlying prompt content changed (new version): %q == %q", fpV1, fpV2)
	}

	// The OLD, pinned version's own fingerprint must still reproduce
	// exactly what it always did -- fingerprintFor is derived fresh from
	// Get's result, so this is automatic, but worth asserting directly.
	_, fpV1Again, _, err := s.Resolve("greeting", 1, nil)
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
				_, _, _, _ = s.Resolve(id, 0, map[string]string{"x": "y"})
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

// TestGetLatestWithOutOfOrderPersistedVersions documents a real, current
// behavioral quirk rather than proving a bug: Get's own "latest" logic
// (version <= 0) returns versions[len(versions)-1] -- the LAST SLICE
// ELEMENT, never the entry with the maximum Version field. In-memory
// Upsert always appends in ascending order, so this never bites via that
// path, but a Persister.Load() implementation that returns a
// DIFFERENTLY-ordered slice (out of order, or even just reversed) would
// make "latest" resolve to whatever happens to be last in that slice,
// not the highest version -- pinned down here explicitly so a future
// intentional fix (defensively sorting on load, or documenting that
// Persister implementations must return ascending order) has a test to
// update, rather than discovering this from scratch.
func TestGetLatestWithOutOfOrderPersistedVersions(t *testing.T) {
	fp := &fakePersister{
		data: map[string][]Prompt{
			"greeting": {
				{ID: "greeting", Version: 2, Messages: msgs("v2-content")},
				{ID: "greeting", Version: 1, Messages: msgs("v1-content")},
			},
		},
	}
	s, err := NewStoreWithPersister(context.Background(), fp)
	if err != nil {
		t.Fatalf("NewStoreWithPersister: %v", err)
	}

	got, ok := s.Get("greeting", 0)
	if !ok {
		t.Fatalf("Get(greeting, 0): not found")
	}
	// Documents CURRENT behavior: the last slice element (Version 1) is
	// returned as "latest," even though Version 2 is the genuinely
	// higher version number -- almost certainly not the intended
	// contract, but this is what Get actually does today.
	if got.Version != 1 {
		t.Errorf("Get(greeting, 0).Version = %d, want 1 (the last slice element, not the highest Version field — see this test's own doc comment)", got.Version)
	}
	if got.Messages[0].Content != "v1-content" {
		t.Errorf("Get(greeting, 0).Messages[0].Content = %q, want %q", got.Messages[0].Content, "v1-content")
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

// fakePersister is a minimal in-memory Persister for tests -- exercises
// Store's own Load/Save/Close call sites without a real bbolt file; see
// internal/prompt/boltstore for the real implementation.
type fakePersister struct {
	mu      sync.Mutex
	closed  bool
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

func (f *fakePersister) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// TestCloseIsANoOpWithoutAPersister proves Close mirrors
// budget.Tracker.Close/identity's Store.Close convention: safe to call on
// a pure in-memory Store (NewStore, no persister configured).
func TestCloseIsANoOpWithoutAPersister(t *testing.T) {
	s := NewStore()
	if err := s.Close(); err != nil {
		t.Errorf("Close on a pure in-memory Store: %v, want nil", err)
	}
}

// TestCloseDelegatesToThePersister proves a configured persister's own
// Close is actually called, not silently skipped.
func TestCloseDelegatesToThePersister(t *testing.T) {
	fp := &fakePersister{data: map[string][]Prompt{}}
	s, err := NewStoreWithPersister(context.Background(), fp)
	if err != nil {
		t.Fatalf("NewStoreWithPersister: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fp.mu.Lock()
	closed := fp.closed
	fp.mu.Unlock()
	if !closed {
		t.Error("Store.Close did not call through to the persister's own Close")
	}
}

// TestSetLabelMovesPointerToAnExistingVersion proves the core promote/
// rollback primitive: SetLabel moves a label to name any existing
// version, forward or backward, and ResolveLabel reflects it
// immediately.
func TestSetLabelMovesPointerToAnExistingVersion(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("v1")); err != nil {
		t.Fatalf("Upsert v1: %v", err)
	}
	if _, err := s.Upsert("greeting", msgs("v2")); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}
	if _, err := s.Upsert("greeting", msgs("v3")); err != nil {
		t.Fatalf("Upsert v3: %v", err)
	}

	l, err := s.SetLabel("greeting", "production", 2)
	if err != nil {
		t.Fatalf("SetLabel(production, 2): %v", err)
	}
	if l.Version != 2 {
		t.Errorf("SetLabel returned Version = %d, want 2", l.Version)
	}
	resolved, _, version, err := s.ResolveLabel("greeting", "production", nil)
	if err != nil {
		t.Fatalf("ResolveLabel: %v", err)
	}
	if version != 2 || resolved[0].Content != "v2" {
		t.Errorf("ResolveLabel = (version=%d, content=%q), want (2, \"v2\")", version, resolved[0].Content)
	}

	// Rollback: move the SAME label to an OLDER version -- the identical
	// operation, no separate verb.
	if _, err := s.SetLabel("greeting", "production", 1); err != nil {
		t.Fatalf("SetLabel(production, 1) (rollback): %v", err)
	}
	resolved, _, version, err = s.ResolveLabel("greeting", "production", nil)
	if err != nil {
		t.Fatalf("ResolveLabel after rollback: %v", err)
	}
	if version != 1 || resolved[0].Content != "v1" {
		t.Errorf("ResolveLabel after rollback = (version=%d, content=%q), want (1, \"v1\")", version, resolved[0].Content)
	}
}

// TestSetLabelRejectsANonExistentVersion proves a label can never point
// at a version that doesn't exist.
func TestSetLabelRejectsANonExistentVersion(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("v1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.SetLabel("greeting", "production", 99); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("SetLabel(production, 99): err = %v, want ErrPromptNotFound", err)
	}
	if _, err := s.SetLabel("does-not-exist", "production", 1); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("SetLabel(unknown id): err = %v, want ErrPromptNotFound", err)
	}
}

// TestSetLabelWithNonPositiveVersionResolvesToLatestAtCallTime proves
// version<=0 resolves to a CONCRETE version number at call time, stored
// as that number -- never a live "latest" sentinel that would silently
// drift if a later Upsert changes what "latest" means.
func TestSetLabelWithNonPositiveVersionResolvesToLatestAtCallTime(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("v1")); err != nil {
		t.Fatalf("Upsert v1: %v", err)
	}
	if _, err := s.Upsert("greeting", msgs("v2")); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}

	l, err := s.SetLabel("greeting", "production", 0)
	if err != nil {
		t.Fatalf("SetLabel(production, 0): %v", err)
	}
	if l.Version != 2 {
		t.Fatalf("SetLabel(0) resolved Version = %d, want 2 (the latest at call time)", l.Version)
	}

	// A later Upsert creates v3 -- the label must NOT silently follow.
	if _, err := s.Upsert("greeting", msgs("v3")); err != nil {
		t.Fatalf("Upsert v3: %v", err)
	}
	_, _, version, err := s.ResolveLabel("greeting", "production", nil)
	if err != nil {
		t.Fatalf("ResolveLabel: %v", err)
	}
	if version != 2 {
		t.Errorf("ResolveLabel after a later Upsert = version %d, want 2 (label pinned at call time, not live)", version)
	}
}

// TestResolveLabelReadsLabelAndVersionFromOneConsistentSnapshot is the
// regression proof for a real bug an audit found: ResolveLabel used to
// read the label's own version via one s.state.Load(), then delegate to
// Resolve, which performed a SECOND, entirely independent s.state.Load()
// internally -- unlike every other method in this file, which each read
// state via exactly one Load() per attempt. A concurrent Delete(id)
// followed by two Upsert(id, ...) calls landing in the gap between those
// two Loads restarts id's version numbering at 1 (Upsert's own doc
// comment), so a stale second Load could silently resolve to a
// completely different, newer prompt "generation" sharing the same bare
// version number, with no error at all.
//
// atomic.Pointer's own CompareAndSwap contract guarantees the *storeState
// a single Load() returns is never mutated in place -- every mutator in
// this file installs a brand-new struct via CAS, never touching the old
// one (confirmed directly in Upsert/Delete/SetLabel/DeleteLabel above).
// This test proves that guarantee both ways, deterministically, with no
// goroutines/timing needed: the SAME captured snapshot, used for both the
// label lookup and the version lookup (exactly what the real, fixed
// ResolveLabel does today), stays correct even after a real mutation
// completes on the live store -- while a hand-simulated SECOND,
// independent Load for the version half (the old, buggy shape) provably
// does NOT.
func TestResolveLabelReadsLabelAndVersionFromOneConsistentSnapshot(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("p1", msgs("original-v1")); err != nil {
		t.Fatalf("seed Upsert v1: %v", err)
	}
	if _, err := s.Upsert("p1", msgs("original-v2")); err != nil {
		t.Fatalf("seed Upsert v2: %v", err)
	}
	if _, err := s.SetLabel("p1", "production", 2); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}

	// Mirrors ResolveLabel's own single Load -- this ONE snapshot is what
	// the fixed implementation uses for both the label and version reads.
	snapshot := s.state.Load()
	l, ok := snapshot.labels["p1"]["production"]
	if !ok || l.Version != 2 {
		t.Fatalf("setup: captured label = %+v, ok=%v, want version 2", l, ok)
	}

	// The real concurrent mutation the audit's own interleaving found: a
	// Delete followed by two fresh Upserts, restarting "p1"'s version
	// numbering at 1 for an entirely new, unrelated prompt generation.
	if err := s.Delete("p1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Upsert("p1", msgs("UNRELATED-new-v1")); err != nil {
		t.Fatalf("re-Upsert v1: %v", err)
	}
	if _, err := s.Upsert("p1", msgs("UNRELATED-new-v2")); err != nil {
		t.Fatalf("re-Upsert v2: %v", err)
	}

	// Sanity check that the mutation actually landed, and that a SECOND,
	// independent Load for the version half (the OLD buggy shape) really
	// does reproduce the exact mismatch the audit found -- if this
	// doesn't hold, the rest of this test would be proving nothing.
	staleP, staleOK := getFromVersions(s.state.Load().versions["p1"], l.Version)
	if !staleOK || staleP.Messages[0].Content != "UNRELATED-new-v2" {
		t.Fatalf("setup: a second, independent Load for version %d = %+v, ok=%v -- want the unrelated new-v2 content (test scenario itself is broken)", l.Version, staleP, staleOK)
	}

	// The load-bearing assertion: resolving the SAME label's version from
	// the ORIGINAL, single captured snapshot -- exactly what the real,
	// fixed ResolveLabel does -- must still return the ORIGINAL content,
	// completely unaffected by the mutation that happened after it was
	// captured.
	p, ok := getFromVersions(snapshot.versions["p1"], l.Version)
	if !ok || p.Messages[0].Content != "original-v2" {
		t.Errorf("resolving from the single captured snapshot = %+v, ok=%v, want the original v2 content -- a single Load must be immune to a later mutation", p, ok)
	}

	// And the real ResolveLabel call, made AFTER the mutation with a
	// FRESH label lookup, correctly reflects reality: "production" was
	// cleared by Delete and never reset, so this must fail loudly, never
	// silently returning either generation's content.
	_, _, _, err := s.ResolveLabel("p1", "production", nil)
	if !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("ResolveLabel after the real delete+recreate cycle = %v, want ErrPromptNotFound", err)
	}
}

// TestResolveLabelUnderConcurrentDeleteRecreateNeverReturnsVersionNotFoundForAKnownLabel
// is the DIFFERENTIAL regression proof for the same TOCTOU bug the
// deterministic test above documents -- that one proves the underlying
// atomic.Pointer/CAS immutability property in isolation, but never
// actually calls the real ResolveLabel during its race window, so
// reverting ResolveLabel to the old, buggy two-Load shape does not make
// it fail. This test does, via a real, live race through the actual
// public API.
//
// The invariant this exploits: storeState guarantees that whenever a
// label names some version V for id WITHIN ONE SNAPSHOT, that same
// snapshot's versions[id] necessarily contains version V too -- SetLabel
// only ever installs a label after confirming the target version exists
// in the very state it's about to CAS into, and Delete clears a prompt's
// versions and its labels together, in the same atomic swap (see
// SetLabel/Delete's own doc comments). So for the FIXED, single-Load
// ResolveLabel, "the label lookup succeeded but the version lookup
// failed" is structurally impossible. For the OLD, buggy two-Load shape,
// it's exactly what happens if a concurrent Delete+re-Upsert cycle lands
// in the gap between the two independent Loads: the first Load catches a
// label from the OLD generation (version 2), and the second, later Load
// (inside the old Resolve delegation) catches a moment after Delete but
// before the new generation's version 2 has been re-created.
//
// A transient "label %q" not-found error is expected and harmless (a
// legitimate Load can land in the brief window after Delete but before
// SetLabel re-establishes the label) -- only the "version %d" shape is
// diagnostic, and must never occur.
func TestResolveLabelUnderConcurrentDeleteRecreateNeverReturnsVersionNotFoundForAKnownLabel(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("p1", msgs("seed-v1")); err != nil {
		t.Fatalf("seed Upsert v1: %v", err)
	}
	if _, err := s.Upsert("p1", msgs("seed-v2")); err != nil {
		t.Fatalf("seed Upsert v2: %v", err)
	}
	if _, err := s.SetLabel("p1", "production", 2); err != nil {
		t.Fatalf("seed SetLabel: %v", err)
	}

	const writerIterations = 3000
	done := make(chan struct{})
	var versionNotFoundSeen atomic.Bool

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < writerIterations; i++ {
			if err := s.Delete("p1"); err != nil {
				t.Errorf("Delete #%d: %v", i, err)
				return
			}
			if _, err := s.Upsert("p1", msgs(fmt.Sprintf("gen-%d-v1", i))); err != nil {
				t.Errorf("Upsert v1 #%d: %v", i, err)
				return
			}
			if _, err := s.Upsert("p1", msgs(fmt.Sprintf("gen-%d-v2", i))); err != nil {
				t.Errorf("Upsert v2 #%d: %v", i, err)
				return
			}
			if _, err := s.SetLabel("p1", "production", 2); err != nil {
				t.Errorf("SetLabel #%d: %v", i, err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, _, _, err := s.ResolveLabel("p1", "production", nil); err != nil {
				if !errors.Is(err, ErrPromptNotFound) {
					t.Errorf("ResolveLabel: unexpected non-ErrPromptNotFound error: %v", err)
					continue
				}
				if strings.Contains(err.Error(), "version") {
					versionNotFoundSeen.Store(true)
				}
			}
		}
	}()

	wg.Wait()
	if versionNotFoundSeen.Load() {
		t.Error(`ResolveLabel returned a "version not found" error for a label lookup that itself succeeded -- this is only reachable by reading the label and its target version from two DIFFERENT, inconsistent snapshots (the exact TOCTOU bug this test guards against)`)
	}
}

// TestResolveLabelReturnsErrPromptNotFoundForAnUnknownLabel proves
// ResolveLabel fails loudly, not silently, for a label that was never
// set.
func TestResolveLabelReturnsErrPromptNotFoundForAnUnknownLabel(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("v1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, _, _, err := s.ResolveLabel("greeting", "staging", nil); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("ResolveLabel(unset label): err = %v, want ErrPromptNotFound", err)
	}
}

// TestSetLabelIsLosslessUnderConcurrentUpsertToSameID mirrors
// TestConcurrentUpsertGetResolveUnderRace's own discipline for the new
// label CAS-retry loop: many concurrent SetLabel calls to DIFFERENT
// label names on the SAME id, run under -race, alongside concurrent
// Upserts to that same id — every SetLabel call must land (none lost to
// a racing Upsert's own CompareAndSwap), and every Upsert must still
// land too (the reverse direction).
func TestSetLabelIsLosslessUnderConcurrentUpsertToSameID(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("v1")); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			label := fmt.Sprintf("label-%d", i)
			if _, err := s.SetLabel("greeting", label, 1); err != nil {
				t.Errorf("SetLabel(%q): %v", label, err)
			}
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Upsert("greeting", msgs(fmt.Sprintf("content-%d", i))); err != nil {
				t.Errorf("Upsert #%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		label := fmt.Sprintf("label-%d", i)
		if _, _, _, err := s.ResolveLabel("greeting", label, nil); err != nil {
			t.Errorf("ResolveLabel(%q) after concurrent run: %v -- a concurrent SetLabel call was lost", label, err)
		}
	}
	latest, ok := s.Get("greeting", 0)
	if !ok {
		t.Fatal("Get(greeting, latest) after concurrent run: not found")
	}
	if latest.Version != n+1 { // +1 for the seed Upsert.
		t.Errorf("latest.Version = %d, want %d -- a concurrent Upsert call was lost", latest.Version, n+1)
	}
}

// TestDeletePromptStillDeletesAllVersionsAndTheirLabels proves Delete
// clears a prompt's labels too, not just its version history -- a
// dangling label pointing at a version number that no longer exists
// would otherwise surface as a confusing not-found from ResolveLabel
// rather than from Delete's own, more informative call site.
func TestDeletePromptStillDeletesAllVersionsAndTheirLabels(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("v1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.SetLabel("greeting", "production", 1); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}

	if err := s.Delete("greeting"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, _, _, err := s.ResolveLabel("greeting", "production", nil); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("ResolveLabel after Delete: err = %v, want ErrPromptNotFound", err)
	}

	// A fresh Upsert under the SAME id must start completely clean --
	// including no leftover "production" label silently reappearing.
	if _, err := s.Upsert("greeting", msgs("new-v1")); err != nil {
		t.Fatalf("Upsert after Delete: %v", err)
	}
	if _, _, _, err := s.ResolveLabel("greeting", "production", nil); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("ResolveLabel after Delete+re-Upsert: err = %v, want ErrPromptNotFound (label must not resurrect)", err)
	}
}

// TestDeleteLabelRemovesItEntirelyWithoutAffectingVersionsOrOtherLabels
// proves DeleteLabel is scoped to exactly one label name, distinct from
// SetLabel-to-an-older-version (which keeps the label, just moved).
func TestDeleteLabelRemovesItEntirelyWithoutAffectingVersionsOrOtherLabels(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("v1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.SetLabel("greeting", "production", 1); err != nil {
		t.Fatalf("SetLabel(production): %v", err)
	}
	if _, err := s.SetLabel("greeting", "staging", 1); err != nil {
		t.Fatalf("SetLabel(staging): %v", err)
	}

	if err := s.DeleteLabel("greeting", "production"); err != nil {
		t.Fatalf("DeleteLabel: %v", err)
	}

	if _, _, _, err := s.ResolveLabel("greeting", "production", nil); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("ResolveLabel(production) after DeleteLabel: err = %v, want ErrPromptNotFound", err)
	}
	if _, _, _, err := s.ResolveLabel("greeting", "staging", nil); err != nil {
		t.Errorf("ResolveLabel(staging) after deleting only production: %v, want nil (staging must be unaffected)", err)
	}
	if _, ok := s.Get("greeting", 1); !ok {
		t.Error("Get(greeting, 1) after DeleteLabel: not found, want the version itself unaffected")
	}
}

// TestDeleteLabelReturnsErrPromptNotFoundForAnUnknownLabel proves
// DeleteLabel fails loudly for a label that was never set, on both an
// unknown id and a known id with no such label.
func TestDeleteLabelReturnsErrPromptNotFoundForAnUnknownLabel(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", msgs("v1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.DeleteLabel("greeting", "staging"); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("DeleteLabel(never-set label): err = %v, want ErrPromptNotFound", err)
	}
	if err := s.DeleteLabel("does-not-exist", "production"); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("DeleteLabel(unknown id): err = %v, want ErrPromptNotFound", err)
	}
}

// TestResolveLabelSubstitutesVariablesIdenticallyToResolve proves
// ResolveLabel delegates to Resolve's own substitution logic rather than
// re-implementing it -- a label-based request gets the identical
// {{name}} placeholder behavior a version-pinned request already has.
func TestResolveLabelSubstitutesVariablesIdenticallyToResolve(t *testing.T) {
	s := NewStore()
	if _, err := s.Upsert("greeting", []adapter.Message{
		{Role: "user", Content: "Hello, {{name}}!"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.SetLabel("greeting", "production", 1); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}

	resolved, _, _, err := s.ResolveLabel("greeting", "production", map[string]string{"name": "Ada"})
	if err != nil {
		t.Fatalf("ResolveLabel: %v", err)
	}
	if resolved[0].Content != "Hello, Ada!" {
		t.Errorf("resolved[0].Content = %q, want %q", resolved[0].Content, "Hello, Ada!")
	}
}
