// Package cache: lexical.go implements the primitives for Cache L3-lite
// (lexical near-duplicate matching via MinHash/Jaccard similarity — never
// embedding-based semantic similarity, per
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md). Deliberately
// pure Go stdlib, no new go.mod entries, per that RFC's "why not real
// embeddings yet" reasoning.
package cache

import (
	"context"
	"hash/fnv"
	"strings"
	"time"
)

// Shingles splits text into overlapping k-word windows — the standard
// MinHash input unit. k must be >= 1; a text with fewer than k words
// yields a single shingle covering the whole text rather than an empty
// set, so short messages still produce a comparable (if coarse)
// signature instead of degenerating to "no shingles at all."
func Shingles(text string, k int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if len(words) <= k {
		return []string{strings.Join(words, " ")}
	}
	shingles := make([]string, 0, len(words)-k+1)
	for i := 0; i+k <= len(words); i++ {
		shingles = append(shingles, strings.Join(words[i:i+k], " "))
	}
	return shingles
}

// splitmix64 is a small, well-known, deterministic bit-mixing function —
// used only to derive MinHashSignature's N hash-permutation coefficients
// from their index, never as a general-purpose PRNG. Same algorithm,
// same output, on every platform and every Go version, forever — unlike
// math/rand, whose algorithm Go's own stdlib does not guarantee stable
// across versions, which would silently break signature comparability
// for any two binaries built with different Go toolchains.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// baseShingleHash is the single real hash function every one of
// MinHashSignature's N permutations is derived from — FNV-64a, stdlib,
// no external dependency.
func baseShingleHash(shingle string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(shingle)) // hash.Hash.Write never errors, per its own documented contract
	return h.Sum64()
}

// MinHashSignature computes an n-value MinHash signature from a shingle
// set: for each of n independent hash permutations, the minimum
// permuted-hash value across every shingle. A nil/empty shingles slice
// returns a signature of all-zero values (a defined, non-panicking
// degenerate case, not a crash) — callers comparing against it will
// correctly see near-zero similarity to any non-empty signature.
func MinHashSignature(shingles []string, n int) []uint64 {
	sig := make([]uint64, n)
	if len(shingles) == 0 {
		return sig
	}
	for i := range sig {
		a := splitmix64(uint64(i)*2 + 1)
		b := splitmix64(uint64(i)*2 + 2)
		min := ^uint64(0) // max uint64
		for _, s := range shingles {
			v := baseShingleHash(s)*a + b // wraps via uint64 overflow — fine for hash mixing
			if v < min {
				min = v
			}
		}
		sig[i] = min
	}
	return sig
}

// JaccardEstimate returns the fraction of matching positions between two
// equal-length MinHash signatures — an unbiased estimator of the true
// Jaccard similarity between their underlying shingle sets. Mismatched
// lengths or a zero-length signature return 0, false rather than
// panicking — a caller must check ok before trusting the estimate.
func JaccardEstimate(a, b []uint64) (estimate float64, ok bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	matches := 0
	for i := range a {
		if a[i] == b[i] {
			matches++
		}
	}
	return float64(matches) / float64(len(a)), true
}

// LexicalCandidate is one Cache L3-lite search hit. Similarity is a
// Jaccard estimate (see JaccardEstimate), never an embedding-cosine
// score — Fingerprint/WrittenAt/ModelID are provenance captured at write
// time, checked by the caller's hard-gate and freshness-risk model
// (dataplane.checkLexicalCache), never inside this package — see
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md's Architecture
// section for why that split matters.
type LexicalCandidate struct {
	Resp        []byte
	Similarity  float64
	Fingerprint map[string]struct{}
	WrittenAt   time.Time
	ModelID     string
}

// LexicalCache is Cache L3-lite's own interface — deliberately not Cache,
// because a similarity search returns zero-to-many scored candidates,
// not a single hit/miss (the same reasoning
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md already applied when
// it rejected widening Cache for L2). Tenant partitioning is enforced
// inside the implementation, never layered on by the caller, per
// THREAT_MODEL.md's KeyPooling mitigation ("baked into the vector-index
// partition itself, not a post-hoc filter").
type LexicalCache interface {
	Search(ctx context.Context, tenantID string, signature []uint64, k int) ([]LexicalCandidate, error)
	Put(ctx context.Context, tenantID string, signature []uint64, resp []byte, fingerprint map[string]struct{}, modelID string, ttl time.Duration) error
}
