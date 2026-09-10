// Package prompt implements the gateway's server-side prompt/template
// management: operator-managed, versioned message templates -- GLOBAL
// config, the same category as price_table/deployments/guardrails config
// (all global today, never tenant-scoped) -- created only via the Admin
// API and resolved into real adapter.Message content at request time, in
// gateway/internal/gateway/dataplane, before routing.
//
// Every prompt ID has its own append-only version history: Upsert always
// creates a NEW version, never edits an existing one in place, so a
// caller that pinned an old version by ID+number keeps getting that
// exact, unchanged content forever.
package prompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync/atomic"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// ErrPromptNotFound is returned by Get/Resolve/Delete when id (or id at
// the requested version) does not match any stored prompt.
var ErrPromptNotFound = errors.New("prompt: prompt not found")

// Prompt is one specific, immutable version of an operator-managed
// prompt template.
type Prompt struct {
	ID        string
	Version   int
	Messages  []adapter.Message
	CreatedAt time.Time
}

// Persister is a seam for future real persistence -- Postgres is the
// named future control-plane store per gateway/ARCHITECTURE.md's Tech
// Stack table ("Control-plane config store | Postgres (pgx/sqlc) --
// still the target for real control-plane state") and its /internal/admin
// section ("Admin mutations are in-memory-only in v1 ... every other
// config section ... stays static-YAML-only"). v1 ships with no real
// implementation of this interface anywhere in this module, mirroring
// internal/budget.Store's own identical separation from its optional
// boltstore backing -- that interface existed and shipped unimplemented
// for a full RFC cycle before internal/budget/boltstore was written
// against it. Save's shape (id + that id's own full version slice, not a
// single Prompt) matches Upsert's own "always the whole history for one
// id" unit of work -- the same granularity budget.Store.Save uses for
// one key's cumulative spend.
type Persister interface {
	Load(ctx context.Context) (map[string][]Prompt, error)
	Save(ctx context.Context, id string, versions []Prompt) error
}

// storeState is the whole-map, rebuild-then-swap unit Store.state holds --
// mirrors identity.Verifier's own "immutable once constructed, swapped as
// a whole" convention (see dataplane.Pipeline.verifier and
// UpsertVirtualKey/DeleteVirtualKey's doc comments): a mutation never
// edits storeState.versions in place, it builds an entirely new map and
// atomically swaps the pointer to it.
type storeState struct {
	versions map[string][]Prompt
}

// Store holds every operator-managed prompt template, keyed by ID, each
// with its own append-only version history. The zero value is not
// usable; construct with NewStore or NewStoreWithPersister.
//
// state is an atomic.Pointer[storeState], rebuild-whole-then-swap -- the
// only established concurrency idiom in this codebase for admin-mutable,
// read-heavy-on-hot-path config data (identity.Verifier /
// dataplane.Pipeline.verifier). Never a sync.Map or a mutex-guarded map.
//
// Unlike identity.Verifier's own UpsertVirtualKey/DeleteVirtualKey (a
// plain Load-then-Store with no retry -- fine for admin traffic's real
// low-concurrency, human-driven write pattern, but not a guarantee that
// holds under genuinely concurrent writers), Upsert/Delete below use
// atomic.Pointer's own CompareAndSwap in a retry loop: still the exact
// same core primitive and the exact same rebuild-whole-then-swap shape,
// but provably lossless under concurrent writers to the same or
// different IDs -- see TestConcurrentUpsertGetResolveUnderRace.
type Store struct {
	state   atomic.Pointer[storeState]
	persist Persister // nil = pure in-memory, the real v1 scope
}

// NewStore constructs an empty, pure in-memory Store.
func NewStore() *Store {
	s := &Store{}
	s.state.Store(&storeState{versions: map[string][]Prompt{}})
	return s
}

// NewStoreWithPersister constructs a Store backed by p: existing prompts
// are loaded immediately, so a restart resumes exactly where it left
// off. A seam only -- no real caller anywhere in this codebase yet,
// mirroring budget.NewTrackerWithStore's own identical position before
// internal/budget/boltstore existed.
func NewStoreWithPersister(ctx context.Context, p Persister) (*Store, error) {
	loaded, err := p.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("prompt: NewStoreWithPersister: %w", err)
	}
	if loaded == nil {
		loaded = map[string][]Prompt{}
	}
	s := &Store{persist: p}
	s.state.Store(&storeState{versions: loaded})
	return s, nil
}

// cloneVersionsMap shallow-copies m's top-level map -- callers always
// replace, never mutate, the []Prompt slice value for whichever id they
// touch (see Upsert/Delete), so sharing every OTHER id's slice header
// here is safe: those slices are never written to in place by anything
// in this package.
func cloneVersionsMap(m map[string][]Prompt) map[string][]Prompt {
	next := make(map[string][]Prompt, len(m))
	for k, v := range m {
		next[k] = v
	}
	return next
}

// cloneMessages returns a fresh slice copy of messages -- Upsert stores
// this, never the caller's own backing array, so a caller mutating its
// slice after passing it in can never retroactively change a stored
// Prompt's content.
func cloneMessages(messages []adapter.Message) []adapter.Message {
	cloned := make([]adapter.Message, len(messages))
	copy(cloned, messages)
	return cloned
}

// Upsert creates a NEW version of id from messages -- version =
// len(existing versions for id) + 1 (1 for a brand-new id). Every prior
// version's own Prompt value is left completely untouched: Upsert only
// ever appends a freshly-allocated slice, never mutates an existing
// []Prompt element or its backing array in place, so a caller holding a
// previously-returned Prompt (or one pinned by ID+version via Get) keeps
// seeing that exact content forever, even while concurrent Upserts to
// the same id keep advancing the "latest" version.
//
// Retries via CompareAndSwap (see Store's own doc comment) rather than a
// plain Load-then-Store: two concurrent Upsert(id, ...) calls for the
// SAME id must never both compute version N+1 and have one silently
// clobber the other -- the loser's stale CAS forces it to recompute
// against the winner's fresh state, so both calls always produce
// distinct, sequential versions with no lost write.
func (s *Store) Upsert(id string, messages []adapter.Message) (Prompt, error) {
	if id == "" {
		return Prompt{}, fmt.Errorf("prompt: Upsert: id must not be empty")
	}
	cloned := cloneMessages(messages)

	for {
		old := s.state.Load()
		existing := old.versions[id]

		p := Prompt{
			ID:        id,
			Version:   len(existing) + 1,
			Messages:  cloned,
			CreatedAt: time.Now(),
		}
		merged := make([]Prompt, len(existing)+1)
		copy(merged, existing)
		merged[len(existing)] = p

		next := cloneVersionsMap(old.versions)
		next[id] = merged

		if !s.state.CompareAndSwap(old, &storeState{versions: next}) {
			continue // lost the race to a concurrent writer -- retry against fresh state
		}
		if s.persist != nil {
			if err := s.persist.Save(context.Background(), id, merged); err != nil {
				return p, fmt.Errorf("prompt: Upsert: persisting %q: %w", id, err)
			}
		}
		return p, nil
	}
}

// Get returns id's prompt at version (version <= 0 means "latest"), or
// false if id (or that specific version of id) does not exist.
func (s *Store) Get(id string, version int) (Prompt, bool) {
	versions := s.state.Load().versions[id]
	if len(versions) == 0 {
		return Prompt{}, false
	}
	if version <= 0 {
		return versions[len(versions)-1], true
	}
	for _, p := range versions {
		if p.Version == version {
			return p, true
		}
	}
	return Prompt{}, false
}

// List returns the latest version of every stored prompt, sorted by ID
// for deterministic output.
func (s *Store) List() []Prompt {
	versions := s.state.Load().versions
	result := make([]Prompt, 0, len(versions))
	for _, vs := range versions {
		if len(vs) == 0 {
			continue
		}
		result = append(result, vs[len(vs)-1])
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Delete removes every version of id. Returns ErrPromptNotFound, changing
// nothing, if id does not exist.
//
// Persister has no delete-specific method (see its own doc comment --
// Load/Save only, since it has no real implementation to design a richer
// contract against yet); a successful Delete persists removal as
// Save(ctx, id, nil) -- Get/List/Resolve already treat a present-but-empty
// versions slice identically to an absent one, so a Persister that
// reloads {id: nil} on restart reproduces "id was deleted" correctly with
// no special-cased empty-slice handling anywhere else in this package.
func (s *Store) Delete(id string) error {
	for {
		old := s.state.Load()
		if len(old.versions[id]) == 0 {
			return fmt.Errorf("%w: %q", ErrPromptNotFound, id)
		}

		next := cloneVersionsMap(old.versions)
		delete(next, id)

		if !s.state.CompareAndSwap(old, &storeState{versions: next}) {
			continue
		}
		if s.persist != nil {
			if err := s.persist.Save(context.Background(), id, nil); err != nil {
				return fmt.Errorf("prompt: Delete: persisting removal of %q: %w", id, err)
			}
		}
		return nil
	}
}

// placeholderPattern matches a "{{name}}" substitution point -- name is
// restricted to [A-Za-z0-9_] (an explicit allowlist of the identifier
// shape a variable name can take), with optional whitespace immediately
// inside the braces ("{{ name }}") tolerated the way most minimal
// template dialects do.
var placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// substitute performs Resolve's own minimal, explicit "{{name}}"
// allowlist substitution over content. Deliberately NOT text/template:
// that package's range/if/pipeline/custom-func execution semantics would
// hand operator-supplied prompt content (which may itself be edited by
// less-trusted operators than the ones who can touch source code) a real,
// if low-severity, template-injection surface -- the only operation this
// function can ever perform is a literal key-for-value swap for a name
// actually present in variables.
//
// Design decision: an unresolved "{{name}}" placeholder (no matching key
// in variables) is left as literal, UNCHANGED text -- never an error,
// never silently dropped. Rationale, per this feature's own explicit
// design requirement:
//   - Silently dropping it (replacing with "") would turn e.g.
//     "Hello {{name}}!" into "Hello !" whenever a caller simply forgets
//     one variable -- indistinguishable from a real bug/data-loss to any
//     human or downstream consumer, and impossible to notice from the
//     output alone.
//   - Erroring would make prompt resolution a hard, request-time-fragile
//     dependency requiring every template edit and every call site's
//     variable-passing to be co-ordinated in perfect lockstep before a
//     single change is safe to deploy -- a strong coupling this
//     feature's own "any virtual key may reference any prompt_id"
//     global-config design deliberately avoids.
//   - Leaving the literal text in place keeps a caller's mistake VISIBLE
//     (an obviously-unsubstituted "{{typo_name}}" in the response is
//     cheap to notice and fix) without ever taking the request down or
//     silently corrupting it.
func substitute(content string, variables map[string]string) string {
	if len(variables) == 0 {
		return content
	}
	return placeholderPattern.ReplaceAllStringFunc(content, func(match string) string {
		name := match[2 : len(match)-2]
		// Trim the same optional inner whitespace placeholderPattern's
		// own \s* already tolerated, so "{{ name }}" resolves against
		// variables["name"], not variables[" name "].
		for len(name) > 0 && (name[0] == ' ' || name[0] == '\t') {
			name = name[1:]
		}
		for len(name) > 0 && (name[len(name)-1] == ' ' || name[len(name)-1] == '\t') {
			name = name[:len(name)-1]
		}
		if v, ok := variables[name]; ok {
			return v
		}
		return match
	})
}

// fingerprintFor derives Resolve's cache-key-fold value from p: p's own
// ID + Version, plus a hash of its STORED (pre-substitution) Messages --
// deliberately NOT the post-substitution resolved output, since the
// resolved messages themselves already flow into the cache key via the
// existing serializeMessages(req.Messages) fold once dataplane sets
// req.Messages to Resolve's result. This fingerprint's own job is
// narrower: a stable identity for WHICH template (id+version) produced
// that content, per this feature's cache-key design.
//
// Deriving fresh from whatever Get returns on every call is what makes
// this "automatically correct with no extra bookkeeping": stable for
// repeated calls against the same version (Get returns the identical
// Prompt value every time), and different the instant a NEW version
// exists for id (Upsert never edits an existing version's content in
// place, so this is the only way stored content ever changes at all).
func fingerprintFor(p Prompt) string {
	b, err := json.Marshal(p.Messages)
	if err != nil {
		// encoding/json cannot fail on this struct shape (no channels/
		// funcs/unsupported map key types) -- mirrors
		// dataplane.serializeMessages' identical panic-on-marshal-failure
		// convention for the same reason: a failure here indicates a bug
		// in the canonical adapter.Message shape, not a runtime condition
		// callers should have to handle.
		panic(fmt.Sprintf("prompt: marshaling messages for fingerprint: %v", err))
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%s:v%d:%s", p.ID, p.Version, hex.EncodeToString(sum[:]))
}

// Resolve looks up id at version (<= 0 means latest), substitutes
// variables into every message's text content (see substitute's own doc
// comment for the unresolved-placeholder design decision), and returns
// the result alongside a stable fingerprint for the cache-key fold (see
// fingerprintFor). Returns ErrPromptNotFound (wrapped with id/version
// context) if no such prompt/version exists.
func (s *Store) Resolve(id string, version int, variables map[string]string) ([]adapter.Message, string, error) {
	p, ok := s.Get(id, version)
	if !ok {
		if version > 0 {
			return nil, "", fmt.Errorf("%w: %q version %d", ErrPromptNotFound, id, version)
		}
		return nil, "", fmt.Errorf("%w: %q", ErrPromptNotFound, id)
	}

	resolved := make([]adapter.Message, len(p.Messages))
	for i, m := range p.Messages {
		m.Content = substitute(m.Content, variables)
		// A multi-modal message's own lead-in/trailing text lives in
		// Parts[i].Text (Type == "text"), independently of Content --
		// see adapter.Message's own doc comment ("Content's own type is
		// unchanged... a multi-modal message may still carry lead-in
		// text here"). Substitution must reach both, or a template
		// combining an image part with a "{{name}}"-bearing text part
		// would silently skip the text part's own placeholders.
		if len(m.Parts) > 0 {
			parts := make([]adapter.ContentPart, len(m.Parts))
			copy(parts, m.Parts)
			for j, part := range parts {
				if part.Type == "text" {
					part.Text = substitute(part.Text, variables)
					parts[j] = part
				}
			}
			m.Parts = parts
		}
		resolved[i] = m
	}
	return resolved, fingerprintFor(p), nil
}
