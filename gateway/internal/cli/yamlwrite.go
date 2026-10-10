package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// yamlwrite.go is the offline writer behind `kelvran keys` (RFC-3 decision
// 3). It edits the virtual_keys.<name> block of a config file as a text
// operation, classifying lines exactly as the loader's parseYAMLMini does
// (controlplane.StripYAMLComment, FindKeyColon, UnquoteYAMLScalar) and
// preserving every byte it does not mean to change. The caller proves the
// result with the semantic Load-delta check before writeInPlace.

// yamlLine is one source line: comment/blank, or content with its indent,
// unquoted key, trimmed value and the raw index of the key/value colon.
type yamlLine struct {
	raw     string
	content bool
	indent  int
	key     string
	value   string
	colon   int
}

type yamlDoc struct {
	lines []yamlLine
	eol   string // line ending for inserted lines: "\n", or "\r\n" when the file uses it
}

// parseYAMLDoc classifies every line of data as the loader does; a leading
// tab or a content line without a colon is refused, as Load refuses them.
func parseYAMLDoc(data []byte) (*yamlDoc, error) {
	d := &yamlDoc{eol: "\n"}
	for i, raw := range strings.Split(string(data), "\n") {
		l := yamlLine{raw: raw}
		stripped := controlplane.StripYAMLComment(raw)
		trimmedRight := strings.TrimRight(stripped, " \t\r")
		if strings.TrimSpace(trimmedRight) != "" {
			leading := trimmedRight[:len(trimmedRight)-len(strings.TrimLeft(trimmedRight, " \t"))]
			if strings.ContainsRune(leading, '\t') {
				return nil, fmt.Errorf("line %d: leading tab characters are not valid YAML indentation (the loader refuses this file too)", i+1)
			}
			text := strings.TrimSpace(trimmedRight)
			ci := controlplane.FindKeyColon(text)
			if ci < 0 {
				return nil, fmt.Errorf("line %d: expected \"key: value\" or \"key:\" (the loader refuses this file too)", i+1)
			}
			l.content, l.indent = true, len(leading)
			l.key = controlplane.UnquoteYAMLScalar(strings.TrimSpace(text[:ci]))
			l.value = strings.TrimSpace(text[ci+1:])
			l.colon = l.indent + ci
			if strings.HasSuffix(raw, "\r") {
				d.eol = "\r\n"
			}
		}
		d.lines = append(d.lines, l)
	}
	return d, nil
}

// render joins the lines back; with no insertion the bytes are the input's.
func (d *yamlDoc) render() []byte {
	parts := make([]string, len(d.lines))
	for i, l := range d.lines {
		parts[i] = l.raw
	}
	return []byte(strings.Join(parts, "\n"))
}

// keyBlock locates the virtual_keys section: the unique content line at
// indent 0 with that key and an empty value; its entries are the content
// lines at the entry indent E (read from the file, never assumed), each
// spanning to the last content line before the next line at indent <= E.
type keyBlock struct {
	section     int
	end         int // index of the first indent-0 content line after the block, or len(lines)
	lastContent int // last content line inside the block (section itself when empty)
	entryIndent int // E; 0 when the block has no entries
	childIndent int // C: indent of the entries' own keys; 0 when unknown
	entries     []keyEntry
}

type keyEntry struct {
	name        string
	start       int
	lastContent int
}

func (d *yamlDoc) virtualKeysBlock() (*keyBlock, error) {
	section := -1
	for i, l := range d.lines {
		if l.content && l.indent == 0 && l.key == "virtual_keys" && l.value == "" {
			if section >= 0 {
				return nil, errors.New("more than one virtual_keys section at indent 0; edit the file by hand")
			}
			section = i
		}
	}
	if section < 0 {
		return nil, errors.New("no virtual_keys section at indent 0 (a `virtual_keys:` line with nothing after the colon); edit the file by hand")
	}
	b := &keyBlock{section: section, end: len(d.lines), lastContent: section}
	for i := section + 1; i < len(d.lines); i++ {
		l := d.lines[i]
		if !l.content {
			continue
		}
		if l.indent == 0 {
			b.end = i
			break
		}
		if b.entryIndent == 0 {
			b.entryIndent = l.indent
		}
		if l.indent < b.entryIndent {
			return nil, fmt.Errorf("line %d: irregular indentation under virtual_keys (%d spaces where the entries use %d); edit the file by hand", i+1, l.indent, b.entryIndent)
		}
		b.lastContent = i
		if l.indent == b.entryIndent {
			b.entries = append(b.entries, keyEntry{name: l.key, start: i, lastContent: i})
			continue
		}
		if len(b.entries) > 0 {
			b.entries[len(b.entries)-1].lastContent = i
			if b.childIndent == 0 {
				b.childIndent = l.indent
			}
		}
	}
	return b, nil
}

func (b *keyBlock) find(name string) (keyEntry, bool) {
	for _, e := range b.entries {
		if e.name == name {
			return e, true
		}
	}
	return keyEntry{}, false
}

// deleteEntry removes the entry's span: its line through its last content
// line, taking the comment and blank lines strictly inside with it (the
// parser never sees them, so they belong to the entry); comment lines above
// the entry and blank/comment lines after the span stay byte for byte.
func (d *yamlDoc) deleteEntry(name string) (*yamlDoc, error) {
	b, err := d.virtualKeysBlock()
	if err != nil {
		return nil, err
	}
	e, ok := b.find(name)
	if !ok {
		return nil, fmt.Errorf("virtual key %q has no entry in the virtual_keys block", name)
	}
	out := &yamlDoc{eol: d.eol}
	out.lines = append(out.lines, d.lines[:e.start]...)
	out.lines = append(out.lines, d.lines[e.lastContent+1:]...)
	return out, nil
}

// rotateEntry replaces only the value token of the entry's key_hash line —
// the first whitespace-delimited token after the colon, re-quoted as the old
// one was — leaving colon spacing and any trailing comment intact. With a
// non-empty expiresAt it also rewrites the entry's expires_at token, or
// inserts an expires_at line after key_hash when the entry has none.
func (d *yamlDoc) rotateEntry(name, newHash, expiresAt string) (*yamlDoc, error) {
	b, err := d.virtualKeysBlock()
	if err != nil {
		return nil, err
	}
	e, ok := b.find(name)
	if !ok {
		return nil, fmt.Errorf("virtual key %q has no entry in the virtual_keys block", name)
	}
	out := &yamlDoc{eol: d.eol, lines: append([]yamlLine(nil), d.lines...)}
	child := entryChildIndent(d, e)
	if child == 0 {
		return nil, fmt.Errorf("virtual key %q has no nested fields (the loader would refuse the file: key_hash is required)", name)
	}
	hashLine, expLine := -1, -1
	for i := e.start + 1; i <= e.lastContent; i++ {
		l := d.lines[i]
		if !l.content || l.indent != child {
			continue
		}
		switch l.key {
		case "key_hash":
			if hashLine < 0 {
				hashLine = i
			}
		case "expires_at":
			if expLine < 0 {
				expLine = i
			}
		}
	}
	if hashLine < 0 {
		return nil, fmt.Errorf("virtual key %q has no key_hash line at its field indent", name)
	}
	raw, err := replaceValueToken(d.lines[hashLine].raw, d.lines[hashLine].colon, newHash)
	if err != nil {
		return nil, fmt.Errorf("virtual key %q key_hash: %w", name, err)
	}
	out.lines[hashLine].raw = raw
	if expiresAt != "" {
		switch {
		case expLine >= 0:
			raw, err := replaceValueToken(d.lines[expLine].raw, d.lines[expLine].colon, expiresAt)
			if err != nil {
				return nil, fmt.Errorf("virtual key %q expires_at: %w", name, err)
			}
			out.lines[expLine].raw = raw
		default:
			line := yamlLine{raw: strings.Repeat(" ", child) + "expires_at: \"" + expiresAt + "\"" + strings.TrimSuffix(d.eol, "\n")}
			out.lines = append(out.lines[:hashLine+1], append([]yamlLine{line}, out.lines[hashLine+1:]...)...)
		}
	}
	return out, nil
}

// entryChildIndent is the indent of an entry's own fields: the first content
// line inside its span.
func entryChildIndent(d *yamlDoc, e keyEntry) int {
	for i := e.start + 1; i <= e.lastContent; i++ {
		if d.lines[i].content {
			return d.lines[i].indent
		}
	}
	return 0
}

// replaceValueToken swaps the first whitespace-delimited token after the
// colon for newValue, keeping the old token's quoting, the spacing and any
// trailing comment bytes.
func replaceValueToken(raw string, colon int, newValue string) (string, error) {
	rest := raw[colon+1:]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	j := i
	for j < len(rest) && rest[j] != ' ' && rest[j] != '\t' && rest[j] != '#' && rest[j] != '\r' && rest[j] != '\n' {
		j++
	}
	if i == j {
		return "", errors.New("the line has no value to replace")
	}
	old := rest[i:j]
	quote := ""
	if len(old) >= 2 && (old[0] == '"' || old[0] == '\'') && old[len(old)-1] == old[0] {
		quote = string(old[0])
	}
	return raw[:colon+1] + rest[:i] + quote + newValue + quote + rest[j:], nil
}

// insertEntry appends a new entry immediately after the block's last content
// line — before any trailing blank or comment lines, so column-0 commentary
// introducing the next section stays attached to it — at the entries' indent
// (2 when the block is empty), writing key_hash and only the non-default
// fields of the spec.
func (d *yamlDoc) insertEntry(spec keySpec, hash string) (*yamlDoc, error) {
	b, err := d.virtualKeysBlock()
	if err != nil {
		return nil, runtimeErr("%v", err)
	}
	if _, exists := b.find(spec.name); exists {
		return nil, runtimeErr("virtual key %q already has an entry", spec.name)
	}
	spelled, err := spellEntryName(spec.name)
	if err != nil {
		return nil, err
	}
	entryIndent, childIndent := b.entryIndent, b.childIndent
	if entryIndent == 0 {
		entryIndent = 2
	}
	if childIndent <= entryIndent {
		childIndent = entryIndent * 2
	}
	nl := strings.TrimSuffix(d.eol, "\n")
	pad := func(n int) string { return strings.Repeat(" ", n) }
	var lines []yamlLine
	add := func(s string) { lines = append(lines, yamlLine{raw: s + nl}) }
	add(pad(entryIndent) + spelled + ":")
	add(pad(childIndent) + "key_hash: \"" + hash + "\"")
	if spec.budgetText != "" {
		add(pad(childIndent) + "budget_usd: " + spec.budgetText)
	}
	if spec.resetSeconds != 0 {
		add(fmt.Sprintf("%sbudget_reset_interval_seconds: %d", pad(childIndent), spec.resetSeconds))
	}
	if spec.warnText != "" {
		add(pad(childIndent) + "budget_warn_percent: " + spec.warnText)
	}
	if len(spec.models) > 0 {
		add(pad(childIndent) + "allowed_models:")
		for _, m := range spec.models {
			add(pad(childIndent+(childIndent-entryIndent)) + m + ": true")
		}
	}
	if spec.billing != "" {
		add(pad(childIndent) + "billing_subject_id: \"" + spec.billing + "\"")
	}
	if t := spec.expiresText(); t != "" {
		add(pad(childIndent) + "expires_at: \"" + t + "\"")
	}
	at := b.lastContent + 1
	// Keep the file's own habit of a blank line between entries when the
	// previous line is content.
	if b.lastContent > b.section && d.lines[b.lastContent].content && len(b.entries) > 0 {
		lines = append([]yamlLine{{raw: nl}}, lines...)
	}
	out := &yamlDoc{eol: d.eol}
	out.lines = append(out.lines, d.lines[:at]...)
	out.lines = append(out.lines, lines...)
	out.lines = append(out.lines, d.lines[at:]...)
	return out, nil
}

// spellEntryName writes a key name the way the parser reads it back: bare,
// or double-quoted when it carries a colon or starts with a quote (the one
// spelling FindKeyColon protects). Names the subset cannot represent or would
// silently re-spell are refused with a pointer at the online path.
func spellEntryName(name string) (string, error) {
	refuse := func(why string) (string, error) {
		return "", usageErr("virtual key name %q cannot be written to config.yaml (%s); create it online through the admin API instead", name, why)
	}
	switch {
	case name == "":
		return refuse("empty")
	case strings.TrimSpace(name) != name:
		return refuse("surrounding whitespace")
	case strings.ContainsAny(name, "#\n\r"):
		return refuse("a # or a line break, which the comment stripper would cut")
	case len(name) >= 2 && name[0] == name[len(name)-1] && (name[0] == '"' || name[0] == '\''):
		return refuse("wrapped in quotes, which the parser would strip")
	}
	if strings.Contains(name, ":") || name[0] == '"' || name[0] == '\'' {
		if strings.Contains(name, "\"") {
			return refuse("a double quote inside a name that must be quoted")
		}
		return "\"" + name + "\"", nil
	}
	return name, nil
}

// writeInPlace replaces the file at path (following a symlink to its target)
// atomically, keeping the target's mode and owner: a temp file in the same
// directory, written and synced, chmod to the original mode (CreateTemp opens
// 0600 — never a fixed 0600 for a config the packaged unit's DynamicUser must
// read), chown to the original owner when it differs from this process (EPERM
// is a refusal), then rename and a directory sync. The temp file is removed on
// every error path.
func writeInPlace(path string, data []byte) (mode os.FileMode, target string, err error) {
	target, err = filepath.EvalSymlinks(path)
	if err != nil {
		return 0, "", fmt.Errorf("resolving %s: %w", path, err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		return 0, "", err
	}
	if !fi.Mode().IsRegular() {
		return 0, "", fmt.Errorf("%s is not a regular file", target)
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return 0, "", fmt.Errorf("creating a temp file beside %s: %w", target, err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, e error) (os.FileMode, string, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return 0, "", fmt.Errorf("%s %s: %w", step, target, e)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("writing", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("syncing", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("closing", err)
	}
	mode = fi.Mode().Perm()
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fail("preserving the mode of", err)
	}
	if uid, gid, ok := fileOwner(fi); ok && (uid != os.Getuid() || gid != os.Getgid()) {
		if err := os.Chown(tmpPath, uid, gid); err != nil {
			_ = os.Remove(tmpPath)
			return 0, "", fmt.Errorf("cannot keep %s owned by uid %d gid %d (%w); run as that user or as root", target, uid, gid, err)
		}
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return 0, "", fmt.Errorf("replacing %s: %w", target, err)
	}
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: the config's own directory
		_ = d.Sync() // best effort: some filesystems refuse fsync on a directory
		_ = d.Close()
	}
	return mode, target, nil
}

// jsonOut writes one JSON document to stdout.
func jsonOut(env IO, v any) error {
	if err := json.NewEncoder(env.Stdout).Encode(v); err != nil {
		return runtimeErr("writing output: %v", err)
	}
	return nil
}
