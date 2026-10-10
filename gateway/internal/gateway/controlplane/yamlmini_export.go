package controlplane

// Exported, stdlib-only views of parseYAMLMini's three line-level helpers,
// for the kelvran CLI's offline virtual_keys writer (RFC-3 decision 3): the
// writer classifies lines exactly as the loader does, so the two cannot
// disagree about what is a comment, a key or a value. They carry no
// behaviour of their own; the writer's real safety net is loading the
// rewritten document and comparing it with the original.

// StripYAMLComment is stripYAMLComment: the line up to its first "#".
func StripYAMLComment(line string) string { return stripYAMLComment(line) }

// FindKeyColon is findKeyColon: the index of the colon separating a content
// line's key from its value, or -1 when there is none.
func FindKeyColon(content string) int { return findKeyColon(content) }

// UnquoteYAMLScalar is unquoteYAMLScalar: one layer of matching quotes removed.
func UnquoteYAMLScalar(s string) string { return unquoteYAMLScalar(s) }
