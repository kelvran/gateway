package auditstore

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tamper-evident chain (round-5 Tier S item 26): every appended line
// carries seq and prev_hash (the SHA-256 of the previous line's bytes),
// plus an HMAC-SHA256 over its canonical bytes when a key is configured;
// Verify walks a file and reports the first thing that does not hold.

var testKey = []byte("audit-chain-test-key-not-a-real-secret")

func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a t.TempDir() path this test created
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	raw = bytes.TrimSuffix(raw, []byte("\n"))
	if len(raw) == 0 {
		return nil
	}
	return bytes.Split(raw, []byte("\n"))
}

func writeLines(t *testing.T, path string, lines [][]byte, trailingNewline bool) {
	t.Helper()
	joined := bytes.Join(lines, []byte("\n"))
	if trailingNewline {
		joined = append(joined, '\n')
	}
	if err := os.WriteFile(path, joined, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// expectedMAC recomputes what Append must have written: "v1," plus the
// hex HMAC-SHA256 of the line marshalled with MAC cleared.
func expectedMAC(t *testing.T, raw []byte, key []byte) string {
	t.Helper()
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("Unmarshal %q: %v", raw, err)
	}
	e.MAC = ""
	canonical, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, key)
	m.Write(canonical)
	return "v1," + hex.EncodeToString(m.Sum(nil))
}

func chainedFile(t *testing.T, key []byte, n int) (string, [][]byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := Open(path, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := store.Append("admin_config_read", "authorized_by", "admin", "n", i); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path, readLines(t, path)
}

func verifyFile(t *testing.T, path string, key []byte) Report {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: a t.TempDir() path this test created
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rep, err := Verify(f, key)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep
}

func kinds(rep Report) map[string][]int {
	out := map[string][]int{}
	for _, p := range rep.Problems {
		out[p.Kind] = append(out[p.Kind], p.Line)
	}
	return out
}

func TestAppendChainsLinesWithSeqAndPrevHash(t *testing.T) {
	_, lines := chainedFile(t, nil, 3)
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	for i, raw := range lines {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if e.Seq != uint64(i+1) {
			t.Errorf("line %d: seq = %d, want %d", i+1, e.Seq, i+1)
		}
		wantPrev := ""
		if i > 0 {
			wantPrev = lineHash(lines[i-1])
		}
		if e.PrevHash != wantPrev {
			t.Errorf("line %d: prev_hash = %q, want %q (the SHA-256 of the previous line's bytes; empty for the first line)", i+1, e.PrevHash, wantPrev)
		}
		if e.MAC != "" {
			t.Errorf("line %d: mac = %q, want none without a key", i+1, e.MAC)
		}
		if e.Msg != "admin_config_read" || e.Fields["authorized_by"] != "admin" {
			t.Errorf("line %d: the record itself changed: %+v", i+1, e)
		}
	}
}

func TestAppendReturnsTheHeadOfTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	head1, err := store.Append("admin_config_read", "authorized_by", "admin")
	if err != nil {
		t.Fatal(err)
	}
	head2, err := store.Append("admin_config_read", "authorized_by", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	if head1.Seq != 1 || head1.Hash != lineHash(lines[0]) {
		t.Errorf("head after the first append = %+v, want seq 1 and the first line's hash %s", head1, lineHash(lines[0]))
	}
	if head2.Seq != 2 || head2.Hash != lineHash(lines[1]) {
		t.Errorf("head after the second append = %+v, want seq 2 and the second line's hash", head2)
	}
}

func TestAppendSignsEveryLineWhenAKeyIsConfigured(t *testing.T) {
	_, lines := chainedFile(t, testKey, 2)
	for i, raw := range lines {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		if want := expectedMAC(t, raw, testKey); e.MAC != want {
			t.Errorf("line %d: mac = %q, want %q (v1, hex HMAC-SHA256 over the line with mac cleared)", i+1, e.MAC, want)
		}
		if !strings.HasPrefix(e.MAC, "v1,") {
			t.Errorf("line %d: mac %q lacks the v1, prefix", i+1, e.MAC)
		}
	}
}

func TestOpenResumesTheChainAcrossReopen(t *testing.T) {
	path, lines := chainedFile(t, testKey, 2)
	store, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.Append("admin_virtual_key_upserted", "name", "team-alpha")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	all := readLines(t, path)
	if len(all) != 3 || head.Seq != 3 {
		t.Fatalf("after reopen: %d lines, head %+v; want 3 lines and seq 3", len(all), head)
	}
	var e Entry
	if err := json.Unmarshal(all[2], &e); err != nil {
		t.Fatal(err)
	}
	if e.PrevHash != lineHash(lines[1]) {
		t.Errorf("resumed line prev_hash = %q, want the hash of the last pre-existing line %q", e.PrevHash, lineHash(lines[1]))
	}
	if rep := verifyFile(t, path, testKey); !rep.OK() || rep.LastSeq != 3 {
		t.Errorf("Verify after reopen = %+v, want OK with last seq 3", rep)
	}
}

func TestOpenChainsOntoALegacyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	legacy := [][]byte{
		[]byte(`{"time":"2026-09-25T10:00:00Z","msg":"admin_config_read","fields":{"authorized_by":"admin"}}`),
		[]byte(`{"time":"2026-09-25T10:00:01Z","msg":"admin_virtual_key_upserted","fields":{"authorized_by":"admin","name":"old"}}`),
	}
	writeLines(t, path, legacy, true)
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.Append("admin_config_read", "authorized_by", "admin")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	all := readLines(t, path)
	if len(all) != 3 {
		t.Fatalf("lines = %d, want the two legacy lines plus one chained", len(all))
	}
	if !bytes.Equal(all[0], legacy[0]) || !bytes.Equal(all[1], legacy[1]) {
		t.Fatal("the legacy lines were rewritten; Open must never touch existing bytes")
	}
	var e Entry
	if err := json.Unmarshal(all[2], &e); err != nil {
		t.Fatal(err)
	}
	if e.Seq != 1 || head.Seq != 1 {
		t.Errorf("first chained line after a legacy prefix: seq = %d (head %d), want 1", e.Seq, head.Seq)
	}
	if e.PrevHash != lineHash(legacy[1]) {
		t.Errorf("prev_hash = %q, want the hash of the last legacy line %q the only legacy line the chain binds in", e.PrevHash, lineHash(legacy[1]))
	}
	rep := verifyFile(t, path, nil)
	if !rep.OK() || rep.LegacyLines != 2 || rep.ChainedLines != 1 || rep.FirstSeq != 1 {
		t.Errorf("Verify = %+v, want OK with 2 legacy lines and 1 chained line", rep)
	}
}

func TestOpenTerminatesATornLastLineBeforeAppending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	torn := []byte(`{"time":"2026-09-25T10:00:00Z","msg":"admin_config_read","fiel`)
	writeLines(t, path, [][]byte{torn}, false)
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("admin_config_read", "authorized_by", "admin"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	all := readLines(t, path)
	if len(all) != 2 || !bytes.Equal(all[0], torn) {
		t.Fatalf("lines = %q, want the torn line left as is and the new line on its own line", all)
	}
	var e Entry
	if err := json.Unmarshal(all[1], &e); err != nil {
		t.Fatalf("the new line must be well-formed JSON on its own line: %v", err)
	}
	if e.PrevHash != lineHash(torn) {
		t.Errorf("prev_hash = %q, want the hash of the torn line's bytes %q", e.PrevHash, lineHash(torn))
	}
}

func TestVerifyAcceptsAnIntactSignedChain(t *testing.T) {
	path, lines := chainedFile(t, testKey, 3)
	rep := verifyFile(t, path, testKey)
	if !rep.OK() {
		t.Fatalf("Verify = %+v, want OK", rep)
	}
	if rep.Lines != 3 || rep.ChainedLines != 3 || rep.SignedLines != 3 || rep.LegacyLines != 0 || rep.UncheckedMACs != 0 {
		t.Errorf("counts = %+v, want 3 lines, all chained and signed", rep)
	}
	if rep.FirstSeq != 1 || rep.LastSeq != 3 {
		t.Errorf("seq range = %d..%d, want 1..3", rep.FirstSeq, rep.LastSeq)
	}
	if rep.HeadHash != lineHash(lines[2]) {
		t.Errorf("HeadHash = %q, want the last line's hash %q", rep.HeadHash, lineHash(lines[2]))
	}
}

func TestVerifyWithoutAKeyLeavesSignedLinesUnchecked(t *testing.T) {
	path, _ := chainedFile(t, testKey, 3)
	rep := verifyFile(t, path, nil)
	if !rep.OK() {
		t.Fatalf("Verify without a key = %+v, want OK (the chain still holds; the MACs are simply not checked)", rep)
	}
	if rep.UncheckedMACs != 3 || rep.SignedLines != 3 {
		t.Errorf("UncheckedMACs = %d, SignedLines = %d; want 3 and 3", rep.UncheckedMACs, rep.SignedLines)
	}
}

func TestVerifyReportsAnEditedLine(t *testing.T) {
	path, lines := chainedFile(t, testKey, 3)
	edited := bytes.Replace(lines[1], []byte(`"authorized_by":"admin"`), []byte(`"authorized_by":"nobody"`), 1)
	if bytes.Equal(edited, lines[1]) {
		t.Fatal("test setup: the edit did not apply")
	}
	writeLines(t, path, [][]byte{lines[0], edited, lines[2]}, true)
	rep := verifyFile(t, path, testKey)
	k := kinds(rep)
	if rep.OK() || len(k["mac_invalid"]) == 0 || k["mac_invalid"][0] != 2 {
		t.Errorf("an edited line 2 must fail its MAC at line 2: %+v", rep.Problems)
	}
	if len(k["prev_hash"]) == 0 || k["prev_hash"][0] != 3 {
		t.Errorf("line 3's prev_hash must no longer match the edited line 2: %+v", rep.Problems)
	}
	// Without the key the chain alone still catches the edit, one line later.
	if rep := verifyFile(t, path, nil); rep.OK() || len(kinds(rep)["prev_hash"]) == 0 {
		t.Errorf("without a key the edit must still break the chain at line 3: %+v", rep.Problems)
	}
}

func TestVerifyReportsADeletedLine(t *testing.T) {
	path, lines := chainedFile(t, nil, 3)
	writeLines(t, path, [][]byte{lines[0], lines[2]}, true)
	rep := verifyFile(t, path, nil)
	k := kinds(rep)
	if rep.OK() || len(k["seq"]) == 0 || k["seq"][0] != 2 {
		t.Errorf("deleting line 2 must leave a seq gap at the new line 2: %+v", rep.Problems)
	}
	if len(k["prev_hash"]) == 0 || k["prev_hash"][0] != 2 {
		t.Errorf("the surviving line 3 must no longer chain to line 1: %+v", rep.Problems)
	}
}

func TestVerifyReportsAReorderedPair(t *testing.T) {
	path, lines := chainedFile(t, nil, 3)
	writeLines(t, path, [][]byte{lines[0], lines[2], lines[1]}, true)
	rep := verifyFile(t, path, nil)
	k := kinds(rep)
	if rep.OK() || len(k["seq"]) == 0 || len(k["prev_hash"]) == 0 {
		t.Errorf("swapping lines 2 and 3 must break both the sequence and the chain: %+v", rep.Problems)
	}
}

func TestVerifyReportsATruncatedTail(t *testing.T) {
	path, _ := chainedFile(t, testKey, 3)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a t.TempDir() path this test created
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-10], 0o600); err != nil { //nolint:gosec // G703: the same t.TempDir() path, rewritten to simulate a truncation
		t.Fatal(err)
	}
	rep := verifyFile(t, path, testKey)
	k := kinds(rep)
	if rep.OK() || len(k["truncated_tail"]) == 0 || k["truncated_tail"][0] != 3 {
		t.Errorf("a final line cut mid-write must be reported as a truncated tail at line 3: %+v", rep.Problems)
	}
	if rep.LastSeq != 2 {
		t.Errorf("LastSeq = %d, want 2: the torn line is not part of the chain", rep.LastSeq)
	}
}

func TestVerifyReportsALegacyPrefixAndRejectsLegacyAfterTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	legacy := []byte(`{"time":"2026-09-25T10:00:00Z","msg":"admin_config_read","fields":{"authorized_by":"admin"}}`)
	writeLines(t, path, [][]byte{legacy, legacy}, true)
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Append("admin_config_read", "authorized_by", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()
	if rep := verifyFile(t, path, nil); !rep.OK() || rep.LegacyLines != 2 || rep.ChainedLines != 2 {
		t.Fatalf("a legacy prefix followed by a chain must verify: %+v", rep)
	}
	// A legacy-shaped line appended AFTER the chain began is a forgery.
	lines := readLines(t, path)
	writeLines(t, path, append(lines, legacy), true)
	rep := verifyFile(t, path, nil)
	if rep.OK() || len(kinds(rep)["legacy_after_chain"]) == 0 {
		t.Errorf("a line without seq after chained lines must be reported: %+v", rep.Problems)
	}
}

func TestVerifyReportsAForgedLineWithoutAMACAfterSigningBegan(t *testing.T) {
	path, lines := chainedFile(t, testKey, 2)
	// The forger recomputes seq and prev_hash correctly but has no key.
	var e Entry
	if err := json.Unmarshal(lines[1], &e); err != nil {
		t.Fatal(err)
	}
	forged := Entry{Time: e.Time, Msg: "admin_virtual_key_deleted", Fields: map[string]string{"authorized_by": "admin", "name": "victim"}, Seq: 3, PrevHash: lineHash(lines[1])}
	raw, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, path, append(lines, raw), true)
	rep := verifyFile(t, path, testKey)
	k := kinds(rep)
	if rep.OK() || len(k["mac_missing"]) == 0 || k["mac_missing"][0] != 3 {
		t.Errorf("an unsigned line after signing began must be reported at line 3: %+v", rep.Problems)
	}
	if len(k["prev_hash"]) != 0 || len(k["seq"]) != 0 {
		t.Errorf("the forger got seq and prev_hash right, so only the MAC must be reported: %+v", rep.Problems)
	}
}

func TestVerifyReportsAMACMadeWithAnotherKey(t *testing.T) {
	path, _ := chainedFile(t, testKey, 2)
	rep := verifyFile(t, path, []byte("a-different-key"))
	k := kinds(rep)
	if rep.OK() || len(k["mac_invalid"]) != 2 {
		t.Errorf("every line must fail its MAC under the wrong key: %+v", rep.Problems)
	}
}

func TestVerifyReportsAMalformedLine(t *testing.T) {
	path, lines := chainedFile(t, nil, 2)
	writeLines(t, path, [][]byte{lines[0], []byte(`{not json`), lines[1]}, true)
	rep := verifyFile(t, path, nil)
	k := kinds(rep)
	if rep.OK() || len(k["malformed"]) == 0 || k["malformed"][0] != 2 {
		t.Errorf("a line that is not JSON must be reported as malformed at line 2, never skipped: %+v", rep.Problems)
	}
	if rep.Lines != 3 {
		t.Errorf("Lines = %d, want 3: a malformed line still counts", rep.Lines)
	}
}

func TestVerifyRejectsAnOversizedLine(t *testing.T) {
	path, lines := chainedFile(t, nil, 1)
	huge := []byte(`{"time":"2026-09-25T10:00:00Z","msg":"` + strings.Repeat("x", MaxLineBytes) + `"}`)
	writeLines(t, path, [][]byte{lines[0], huge}, true)
	rep := verifyFile(t, path, nil)
	if rep.OK() || len(kinds(rep)["line_too_long"]) == 0 {
		t.Errorf("a line beyond MaxLineBytes must be reported, not silently accepted or scanned forever: %+v", rep.Problems)
	}
}

func TestQueryReadsChainedAndLegacyLinesAlike(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writeLines(t, path, [][]byte{[]byte(`{"time":"2026-09-25T10:00:00Z","msg":"admin_config_read","fields":{"authorized_by":"admin"}}`)}, true)
	store, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("admin_config_read", "authorized_by", "viewer"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	entries, err := Query(path, Filter{Msg: "admin_config_read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Seq != 0 || entries[1].Seq != 1 || entries[1].MAC == "" {
		t.Errorf("Query = %+v, want the legacy line (seq 0) and the chained, signed line (seq 1)", entries)
	}
}

// stripMACsAndRechain removes every mac and recomputes each prev_hash so
// the chain holds again: exactly what a tamperer without the key can do.
func stripMACsAndRechain(t *testing.T, lines [][]byte) [][]byte {
	t.Helper()
	out := make([][]byte, 0, len(lines))
	prev := ""
	for _, raw := range lines {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		e.MAC = ""
		e.PrevHash = prev
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
		prev = lineHash(b)
	}
	return out
}

func verifyFileWith(t *testing.T, path string, opts VerifyOptions) Report {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: a t.TempDir() path this test created
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rep, err := VerifyWithOptions(f, opts)
	if err != nil {
		t.Fatalf("VerifyWithOptions: %v", err)
	}
	return rep
}

func appendUnsigned(t *testing.T, path string, n int) {
	t.Helper()
	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open without a key: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := store.Append("admin_config_read", "authorized_by", "viewer"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyWithAKeyRejectsAChainWhoseMACsWereAllRemoved(t *testing.T) {
	path, lines := chainedFile(t, testKey, 3)
	writeLines(t, path, stripMACsAndRechain(t, lines), true)
	rep := verifyFile(t, path, testKey)
	k := kinds(rep)
	if rep.OK() || len(k["mac_missing"]) != 3 {
		t.Errorf("with a key, every chained line without a mac must be reported: %+v", rep.Problems)
	}
	if len(k["prev_hash"]) != 0 || len(k["seq"]) != 0 {
		t.Errorf("the chain was recomputed correctly, so only the macs must be reported: %+v", rep.Problems)
	}
	if rep.UnsignedChainedLines != 3 || rep.SignedLines != 0 {
		t.Errorf("UnsignedChainedLines = %d, SignedLines = %d; want 3 and 0", rep.UnsignedChainedLines, rep.SignedLines)
	}
	if rep := verifyFile(t, path, nil); !rep.OK() || rep.UnsignedChainedLines != 3 {
		t.Errorf("without a key the stripped file is an ordinary unsigned chain and holds: %+v", rep)
	}
}

func TestVerifyAllowsAnUnsignedPrefixOnlyWhenAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	appendUnsigned(t, path, 2) // chained before the operator introduced a key
	store, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Append("admin_config_read", "authorized_by", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	strict := verifyFile(t, path, testKey)
	k := kinds(strict)
	if strict.OK() || len(k["mac_missing"]) != 2 || k["mac_missing"][0] != 1 || k["mac_missing"][1] != 2 {
		t.Errorf("by default a key makes every unsigned chained line a problem: %+v", strict.Problems)
	}
	lenient := verifyFileWith(t, path, VerifyOptions{Key: testKey, AllowUnsignedPrefix: true})
	if !lenient.OK() || lenient.UnsignedChainedLines != 2 || lenient.SignedLines != 2 {
		t.Errorf("AllowUnsignedPrefix must accept the two lines before the first mac: %+v", lenient)
	}
	// A line without a mac AFTER signing began is never a prefix: the key
	// was removed from the config, or the line was forged.
	appendUnsigned(t, path, 1)
	lenient = verifyFileWith(t, path, VerifyOptions{Key: testKey, AllowUnsignedPrefix: true})
	k = kinds(lenient)
	if lenient.OK() || len(k["mac_missing"]) != 1 || k["mac_missing"][0] != 5 || lenient.UnsignedChainedLines != 3 {
		t.Errorf("an unsigned line after signing began must be reported at line 5 even when a prefix is allowed: %+v", lenient)
	}
}

func TestVerifyReportsANonCanonicalLine(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(raw []byte) []byte
	}{
		{"unknown key injected", func(raw []byte) []byte {
			return append(bytes.TrimSuffix(raw, []byte("}")), []byte(`,"note":"approved by the CISO"}`)...)
		}},
		{"duplicate key placed first", func(raw []byte) []byte {
			return append([]byte(`{"msg":"admin_config_read",`), raw[1:]...)
		}},
		{"re-spaced", func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`":"`), []byte(`": "`), 1)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, lines := chainedFile(t, testKey, 3)
			lines[2] = c.mutate(lines[2]) // the last line: no later prev_hash can catch it
			writeLines(t, path, lines, true)
			rep := verifyFile(t, path, testKey)
			k := kinds(rep)
			if rep.OK() || len(k["non_canonical"]) != 1 || k["non_canonical"][0] != 3 {
				t.Errorf("a line whose bytes are not the gateway's own encoding must be reported at line 3: %+v", rep.Problems)
			}
			if len(k["mac_invalid"]) != 0 {
				t.Errorf("the mac still verifies over the parsed content, so non_canonical must be the only finding: %+v", rep.Problems)
			}
		})
	}
	// An intact file never trips it, whatever the field order the writer saw.
	path, _ := chainedFile(t, testKey, 3)
	if rep := verifyFile(t, path, testKey); len(kinds(rep)["non_canonical"]) != 0 {
		t.Errorf("the gateway's own lines are canonical by construction: %+v", rep.Problems)
	}
}

// downgradeToLegacy strips seq, prev_hash and mac from every line: the
// cheapest rewrite a tamperer without the key can make.
func downgradeToLegacy(t *testing.T, lines [][]byte) [][]byte {
	t.Helper()
	out := make([][]byte, 0, len(lines))
	for _, raw := range lines {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		e.Seq, e.PrevHash, e.MAC = 0, "", ""
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func TestVerifyWithAKeyRejectsAFileWithNoSignedLine(t *testing.T) {
	path, lines := chainedFile(t, testKey, 3)
	writeLines(t, path, downgradeToLegacy(t, lines), true)
	rep := verifyFile(t, path, testKey)
	k := kinds(rep)
	if rep.OK() || len(k["no_signed_lines"]) != 1 || k["no_signed_lines"][0] != 0 || rep.LegacyLines != 3 {
		t.Errorf("with a key, a file in which nothing is signed must be reported as a whole-file problem at line 0: %+v", rep)
	}
	if rep := verifyFileWith(t, path, VerifyOptions{Key: testKey, AllowUnsignedPrefix: true}); rep.OK() {
		t.Errorf("an unsigned prefix is only a prefix when something signed follows: %+v", rep)
	}
	if rep := verifyFile(t, path, nil); !rep.OK() {
		t.Errorf("without a key a legacy-only file is simply a legacy file: %+v", rep)
	}
	// A legacy prefix followed by signed lines is the upgrade path and holds.
	appendSigned, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendSigned.Append("admin_config_read", "authorized_by", "admin"); err != nil {
		t.Fatal(err)
	}
	_ = appendSigned.Close()
	if rep := verifyFile(t, path, testKey); !rep.OK() || rep.LegacyLines != 3 || rep.SignedLines != 1 {
		t.Errorf("a legacy prefix whose last line a signed line binds in verifies: %+v", rep)
	}
	// An empty file is the whole-file-deletion class: only the stdout sink
	// can tell, so Verify has nothing to report.
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	writeLines(t, empty, nil, false)
	if rep := verifyFile(t, empty, testKey); !rep.OK() || rep.Lines != 0 {
		t.Errorf("an empty file has no line to fault: %+v", rep)
	}
}

// The encoder replaces an invalid byte with U+FFFD, which decodes and
// re-encodes to the same three bytes, so Unmarshal then Marshal is a fixed
// point and a line the gateway wrote from a caller's invalid UTF-8 verifies
// under its own mac. TestCanonicalEncodingIsPinned pins that encoding (and
// the escaping of < > & and U+2028) against the toolchain.
func TestAnInvalidUTF8FieldValueStillVerifies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("admin_virtual_key_upserted", "name", "bad\xffname", "authorized_by", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	rep := verifyFile(t, path, testKey)
	if !rep.OK() {
		t.Errorf("a line the gateway wrote must verify under its own key whatever bytes a caller passed: %+v", rep.Problems)
	}
	lines := readLines(t, path)
	if bytes.Contains(lines[0], []byte("\xff")) || !bytes.Contains(lines[0], []byte("bad\uFFFDname")) {
		t.Errorf("the invalid byte must reach the file as U+FFFD, never raw: %q", lines[0])
	}
}

// goldenSignedLine is what the encoder produced on 2026-10-16 for a fixed
// entry under testKey: the canonical form the chain and the macs depend on.
// A toolchain whose encoding/json emits different bytes for the same entry
// would make every historical line non_canonical and mac_invalid under a
// verifier built with it, so that change must fail here, not in an
// operator's cron.
const goldenSignedLine = `{"time":"2026-10-16T12:00:00.5Z","msg":"admin_prompt_label_set","fields":{"authorized_by":"admin","bad":"x�y","label":"\u003cprod\u003e\u0026\u2028","name":"r�d","z":""},"seq":7,"prev_hash":"6ba09505d9d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0","mac":"v1,fefbb6d7655944c6d8d92a222cd8f872d15ee3695bc5bff2d9d734001a663349"}`

func TestCanonicalEncodingIsPinned(t *testing.T) {
	e := Entry{
		Time:     time.Date(2026, 10, 16, 12, 0, 0, 500000000, time.UTC),
		Msg:      "admin_prompt_label_set",
		Fields:   map[string]string{"authorized_by": "admin", "bad": "x\xffy", "label": "<prod>& ", "name": "r�d", "z": ""},
		Seq:      7,
		PrevHash: "6ba09505d9d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0",
	}
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	e.MAC = macOf(testKey, line)
	if line, err = json.Marshal(e); err != nil {
		t.Fatal(err)
	}
	if string(line) != goldenSignedLine {
		t.Fatalf("the canonical encoding changed:\n got  %s\n want %s", line, goldenSignedLine)
	}
	rep, err := Verify(strings.NewReader(goldenSignedLine+"\n"), testKey)
	if err != nil {
		t.Fatal(err)
	}
	if k := kinds(rep); len(k["non_canonical"]) != 0 || len(k["mac_invalid"]) != 0 {
		t.Errorf("the golden line must still be canonical and mac-valid: %+v", rep.Problems)
	}
}

func TestOpenReportsWhatItResumed(t *testing.T) {
	open := func(path string, key []byte) Resumed {
		t.Helper()
		s, err := Open(path, key)
		if err != nil {
			t.Fatal(err)
		}
		r := s.Resumed()
		_ = s.Close()
		return r
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if r := open(empty, testKey); r != (Resumed{}) {
		t.Errorf("a new file resumes nothing: %+v", r)
	}
	unsigned := filepath.Join(t.TempDir(), "unsigned.jsonl")
	appendUnsigned(t, unsigned, 2)
	if r := open(unsigned, testKey); r.Lines != 2 || r.LastSeq != 2 || r.SawMAC {
		t.Errorf("two unsigned chained lines: want Lines 2, LastSeq 2, no mac seen; got %+v", r)
	}
	signed, lines := chainedFile(t, testKey, 3)
	if r := open(signed, testKey); r.Lines != 3 || r.LastSeq != 3 || !r.SawMAC {
		t.Errorf("three signed lines: want Lines 3, LastSeq 3, a mac seen; got %+v", r)
	}
	writeLines(t, signed, downgradeToLegacy(t, lines), true)
	if r := open(signed, testKey); r.Lines != 3 || r.LastSeq != 0 || r.SawMAC {
		t.Errorf("a file rewritten as legacy: want Lines 3, LastSeq 0, no mac seen; got %+v", r)
	}
	// A mac member left on a legacy line does not count: Verify would not
	// count it as signed either, so the startup warning must not be fooled.
	decoy := downgradeToLegacy(t, lines)
	decoy[1] = append(bytes.TrimSuffix(decoy[1], []byte("}")), []byte(`,"mac":"v1,00"}`)...)
	writeLines(t, signed, decoy, true)
	if r := open(signed, testKey); r.SawMAC {
		t.Errorf("a mac on a legacy line is not a signed line: %+v", r)
	}
}

// goldenFirstSignedLine pins the first-line shape Append produces, which
// the populated golden cannot: PrevHash "" and an empty Fields map
// omitted, Seq 1 present. (Append never marshals Seq 0, so the shim's
// legacy omitempty-on-zero-uint64 is not exercised here.)
const goldenFirstSignedLine = `{"time":"2026-10-16T12:00:01Z","msg":"admin_config_read","seq":1,"mac":"v1,c739f7daf1a7b29e8f1ee0bebd24179ec8705cb759a730108fb20160cebec0c7"}`

func TestCanonicalEncodingOfAFirstLineIsPinned(t *testing.T) {
	e := Entry{Time: time.Date(2026, 10, 16, 12, 0, 1, 0, time.UTC), Msg: "admin_config_read", Fields: map[string]string{}, Seq: 1}
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	e.MAC = macOf(testKey, line)
	if line, err = json.Marshal(e); err != nil {
		t.Fatal(err)
	}
	if string(line) != goldenFirstSignedLine {
		t.Fatalf("the canonical encoding of a first line changed:\n got  %s\n want %s", line, goldenFirstSignedLine)
	}
	if bytes.Contains(line, []byte(`"prev_hash"`)) || bytes.Contains(line, []byte(`"fields"`)) || !bytes.Contains(line, []byte(`"seq":1,`)) {
		t.Errorf("an empty prev_hash and an empty fields map are omitted, seq 1 is not: %s", line)
	}
	rep, err := Verify(strings.NewReader(goldenFirstSignedLine+"\n"), testKey)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("the golden first line must verify: %+v", rep.Problems)
	}
}
