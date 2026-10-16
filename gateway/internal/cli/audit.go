package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kelvran/gateway/gateway/internal/admin/auditstore"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

const auditUsage = `usage: kelvran audit verify [--file PATH] [--config PATH] [--key-env NAME] [--allow-unsigned-prefix] [--json]

Walks the durable admin audit trail (admin.audit_log_path, one JSON record per line) and checks
its tamper-evident chain: every chained line's seq follows the previous one, its prev_hash is the
SHA-256 of the previous line's bytes, and, when a key is given, its mac is the HMAC-SHA256 the
gateway wrote. Every line where that does not hold is listed: a truncated tail, a line that is
not JSON, a legacy line after the chain began, a seq gap, a broken link, a line whose bytes are not
the gateway's own encoding of its content, a missing or invalid mac, a key given to a file in which
nothing is signed (reported for the whole file), a line past 1 MiB (verification stops there).
--file PATH: the trail; default admin.audit_log_path from --config. --key-env NAME: the variable
holding the signing key; default the variable admin.audit_signing_secret_env names in --config.
Without a key the chain is checked and signed lines are counted, but their macs are not verified
(stderr says so). With a key every chained line must carry a mac, so a file whose macs were all removed
fails; --allow-unsigned-prefix accepts the lines chained before the key was configured (those before the
first signed line), never a line without a mac after signing began. Run it where the file is: the key
never leaves the host.
--json prints the report as one JSON document. Exit 0 when the chain holds, 1 on any problem or a
file that cannot be read, 2 on a usage error.
`

type auditOptions struct {
	file, config, keyEnv string
	jsonOut              bool
	allowUnsignedPrefix  bool
}

// Audit runs `kelvran audit <subcommand>` -- today only `verify` -- and
// returns the exit code: 0 when the trail verifies, 1 on any problem or a
// runtime failure, 2 on a usage error.
func Audit(args []string, env IO) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, auditUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(env.Stderr, auditUsage)
		return 0
	case "verify":
	default:
		_, _ = fmt.Fprintf(env.Stderr, "kelvran audit: unknown subcommand %q\n", sanitizeCell(args[0]))
		_, _ = fmt.Fprint(env.Stderr, auditUsage)
		return 2
	}
	var o auditOptions
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(escapingWriter{env.Stderr})
	fs.Usage = func() { _, _ = fmt.Fprint(env.Stderr, auditUsage) }
	fs.StringVar(&o.file, "file", "", "the audit trail to verify (default admin.audit_log_path from --config)")
	fs.StringVar(&o.config, "config", "", "config file naming admin.audit_log_path and admin.audit_signing_secret_env")
	fs.StringVar(&o.keyEnv, "key-env", "", "environment variable holding the signing key (default the config's admin.audit_signing_secret_env)")
	fs.BoolVar(&o.jsonOut, "json", false, "print the report as JSON")
	fs.BoolVar(&o.allowUnsignedPrefix, "allow-unsigned-prefix", false, "with a key, accept chained lines without a mac that precede the first signed line (a trail chained before the key was configured)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "kelvran audit verify: unexpected argument %q\n", sanitizeCell(fs.Arg(0)))
		fs.Usage()
		return 2
	}
	return exitWith("kelvran audit verify", runAuditVerify(o, env), env, fs.Usage)
}

// auditReportJSON is the --json document: the Report's counts, the
// chain's range and head, and the problems (an array, never null).
type auditReportJSON struct {
	OK                   bool                 `json:"ok"`
	File                 string               `json:"file"`
	Lines                int                  `json:"lines"`
	LegacyLines          int                  `json:"legacy_lines"`
	ChainedLines         int                  `json:"chained_lines"`
	SignedLines          int                  `json:"signed_lines"`
	UnsignedChainedLines int                  `json:"unsigned_chained_lines"`
	UncheckedMACs        int                  `json:"unchecked_macs"`
	FirstSeq             uint64               `json:"first_seq"`
	LastSeq              uint64               `json:"last_seq"`
	HeadHash             string               `json:"head_hash"`
	Problems             []auditstore.Problem `json:"problems"`
}

func runAuditVerify(o auditOptions, env IO) error {
	file, keyEnv := o.file, o.keyEnv
	if o.config != "" {
		cfgAbs, err := filepath.Abs(o.config)
		if err != nil {
			return runtimeErr("resolving --config %s: %v", o.config, err)
		}
		cfg, err := controlplane.Load(cfgAbs)
		if err != nil {
			return runtimeErr("loading %s: %v", o.config, err)
		}
		if file == "" {
			file = cfg.Admin.AuditLogPath
		}
		if keyEnv == "" {
			keyEnv = cfg.Admin.AuditSigningSecretEnv
		}
	}
	if file == "" {
		return usageErr("pass --file PATH, or --config PATH whose admin.audit_log_path is set")
	}
	var key []byte
	if keyEnv != "" {
		v := env.Getenv(keyEnv)
		if v == "" {
			return runtimeErr("the key variable %s is unset or empty; the gateway refuses to start in that state too", sanitizeCell(keyEnv))
		}
		key = []byte(v)
	}
	if o.allowUnsignedPrefix && len(key) == 0 {
		return usageErr("--allow-unsigned-prefix only means something with a key: pass --key-env NAME, or --config naming admin.audit_signing_secret_env")
	}
	f, err := os.Open(file) //nolint:gosec // G304: the operator's own --file/--config path, opened read-only to verify it
	if err != nil {
		return runtimeErr("%v", err)
	}
	defer func() { _ = f.Close() }()
	rep, err := auditstore.VerifyWithOptions(f, auditstore.VerifyOptions{Key: key, AllowUnsignedPrefix: o.allowUnsignedPrefix})
	if err != nil {
		return runtimeErr("reading %s: %v", sanitizeCell(file), err)
	}
	if rep.UncheckedMACs > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "kelvran audit verify: %d signed lines not checked: pass --key-env NAME (or --config naming admin.audit_signing_secret_env) to verify their macs\n", rep.UncheckedMACs)
	}
	if len(key) > 0 && o.allowUnsignedPrefix && rep.UnsignedChainedLines > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "kelvran audit verify: a key was given but %d chained lines carry no mac: the trail predates the key, or every mac was removed; start a new file when you introduce a key\n", rep.UnsignedChainedLines)
	}
	if rep.Lines == 0 {
		_, _ = fmt.Fprint(env.Stderr, "kelvran audit verify: 0 lines: a new or emptied file; compare with the stdout sink's last audit_seq\n")
	}
	if o.jsonOut {
		doc := auditReportJSON{OK: rep.OK(), File: file, Lines: rep.Lines, LegacyLines: rep.LegacyLines, ChainedLines: rep.ChainedLines, SignedLines: rep.SignedLines, UnsignedChainedLines: rep.UnsignedChainedLines, UncheckedMACs: rep.UncheckedMACs, FirstSeq: rep.FirstSeq, LastSeq: rep.LastSeq, HeadHash: rep.HeadHash, Problems: rep.Problems}
		enc := json.NewEncoder(env.Stdout)
		if err := enc.Encode(doc); err != nil {
			return runtimeErr("writing the report: %v", err)
		}
	} else {
		verdict := "OK"
		if !rep.OK() {
			verdict = "FAIL"
		}
		head := rep.HeadHash
		if len(head) > 12 {
			head = head[:12] + "…"
		}
		_, _ = fmt.Fprintln(env.Stdout, sanitizeCell(fmt.Sprintf("%s: %d lines (%d legacy, %d chained, %d signed), seq %d-%d, head %s: %s", file, rep.Lines, rep.LegacyLines, rep.ChainedLines, rep.SignedLines, rep.FirstSeq, rep.LastSeq, head, verdict)))
		for _, p := range rep.Problems {
			where := fmt.Sprintf("line %d", p.Line)
			if p.Line == 0 {
				where = "file"
			}
			_, _ = fmt.Fprintln(env.Stdout, sanitizeCell(fmt.Sprintf("  %s: %s: %s", where, p.Kind, p.Detail)))
		}
	}
	if !rep.OK() {
		return runtimeErr("%d problem(s) in %s", len(rep.Problems), sanitizeCell(file))
	}
	return nil
}
