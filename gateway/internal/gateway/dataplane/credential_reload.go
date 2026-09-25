package dataplane

// This file closes a specific operational-readiness gap: before it
// existed, every Deployment credential (APIKey for non-bedrock
// providers; AccessKeyID/SecretAccessKey/SessionToken for bedrock) was
// resolved via a ONE-TIME os.Getenv call in cmd/gateway's buildPipeline
// and never touched again for the lifetime of the process. AGENTS.md's
// own Gotchas note recommends operators populate those env vars with a
// short-lived AWS STS session triple rather than a permanent IAM user's
// static keypair, specifically to shrink a future leak's severity — but
// adopting that recommendation meant every affected deployment's calls
// would start failing (ExpiredTokenException/InvalidClientTokenId) once
// the session token lapsed, with a full process restart as the ONLY
// remediation.
//
// IMPORTANT DESIGN NOTE, read before touching this file: a bare
// periodic os.Getenv re-read alone would NOT close this gap. A
// Kubernetes projected Secret volume update rewrites the FILE the
// container's env var was originally populated from — it does not, and
// cannot, reach back into an already-running process's own environ.
// os.Getenv reads that fixed-at-exec-time environ, which never changes
// after the process starts, no matter how many times it's re-read. A
// real file, on the other hand, IS what a projected Secret volume mount
// live-updates on rotation. That's why this package re-reads a FILE
// PATH (DeploymentCredentialFiles below — a brand new, purely additive
// *File config convention, parallel to controlplane.DeploymentConfig's
// pre-existing *Env convention) rather than re-calling os.Getenv on the
// same *Env name.
//
// Deployment is copied BY VALUE throughout this package (returned from
// nextDeployment/nextDeploymentSticky, iterated in ProbeDeployments and
// RunCredentialReloadLoop below, held in Pipeline.deploymentsByName,
// etc.) — see Deployment's own doc comment. A plain string field
// updated in place would only ever update ONE such copy, not the
// dozens that already exist by the time a rotation happens. CredState
// is instead a *atomic.Pointer[DeploymentCredentials]: copying a
// Deployment copies the pointer, not what it points to, so every copy
// shares the exact same underlying holder, and reloadDeploymentCredentials'
// atomic Store is visible to every one of them the instant it happens —
// with no mutex needed on the read side (setUpstreamAuthHeaders, via
// effectiveCredentials) at all.
//
// Deployments that never configure any *File field are completely
// unaffected: CredentialFiles stays at its zero value, CredentialState
// stays nil, and effectiveCredentials falls through to the plain
// APIKey/AccessKeyID/SecretAccessKey/SessionToken fields exactly as
// before this feature existed. A deployment still using only the
// pre-existing *Env convention remains restart-only by design — this is
// an honest, bounded fix, not a claim that env-var-based credentials
// are now hot-reloadable too.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// DeploymentCredentials is an immutable snapshot of a deployment's
// resolved upstream credential values — the same four values
// Deployment's own APIKey/AccessKeyID/SecretAccessKey/SessionToken
// fields hold for every deployment that hasn't opted into file-based
// hot-reload. Never logged in full — see reloadDeploymentCredentials'
// own doc comment.
type DeploymentCredentials struct {
	APIKey          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// DeploymentCredentialFiles is the set of on-disk file paths a
// deployment may configure as its credential source, mirroring
// controlplane.DeploymentConfig's four new *File fields one-for-one. An
// empty string means that particular credential still comes from
// Deployment's own plain field, resolved once at cmd/gateway startup
// exactly as before this feature existed.
type DeploymentCredentialFiles struct {
	APIKey          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// HasAny reports whether f configures at least one file-based
// credential source — cmd/gateway's buildPipeline uses this to decide
// whether a deployment opts into RunCredentialReloadLoop at all, and
// RunCredentialReloadLoop itself uses it to skip every deployment that
// didn't.
func (f DeploymentCredentialFiles) HasAny() bool {
	return f.APIKey != "" || f.AccessKeyID != "" || f.SecretAccessKey != "" || f.SessionToken != ""
}

// NewDeploymentCredentialState builds the shared, atomically-swappable
// credential holder cmd/gateway's buildPipeline wires onto
// Deployment.CredentialState for any deployment whose CredentialFiles
// reports HasAny(). Seeded with initial — the SAME values buildPipeline
// already resolved once at startup (an env var, or an initial file
// read via ReadCredentialFile, per its own resolution order) — so the
// very first request behaves identically whether or not a rotation has
// happened yet.
func NewDeploymentCredentialState(initial DeploymentCredentials) *atomic.Pointer[DeploymentCredentials] {
	state := &atomic.Pointer[DeploymentCredentials]{}
	state.Store(&initial)
	return state
}

// effectiveCredentials returns dep's current credential values: the
// latest snapshot from dep.CredentialState if this deployment opted
// into file-based hot-reload (CredentialState != nil), or dep's own
// plain APIKey/AccessKeyID/SecretAccessKey/SessionToken fields
// otherwise — which is every deployment built before this feature
// existed, and every deployment that still only configures the
// pre-existing *Env convention today. This is the ONLY place
// setUpstreamAuthHeaders (or any future upstream-auth call site) should
// ever read a credential value from — never dep.APIKey/AccessKeyID/
// SecretAccessKey/SessionToken directly, since those four fields go
// stale the instant CredentialState exists for a given deployment.
func (d Deployment) effectiveCredentials() DeploymentCredentials {
	if d.CredentialState != nil {
		if snap := d.CredentialState.Load(); snap != nil {
			return *snap
		}
	}
	return DeploymentCredentials{
		APIKey:          d.APIKey,
		AccessKeyID:     d.AccessKeyID,
		SecretAccessKey: d.SecretAccessKey,
		SessionToken:    d.SessionToken,
	}
}

// ReadCredentialFile reads path and returns its trimmed contents — the
// leading/trailing whitespace strip matters because a Kubernetes
// projected Secret volume's file (and most operator-written rotation
// scripts) commonly ends in a trailing newline that isn't part of the
// actual credential value. Exported so cmd/gateway's buildPipeline can
// use the exact same read logic for a deployment's INITIAL value that
// RunCredentialReloadLoop below uses for every subsequent re-read of
// the same path.
func ReadCredentialFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// DefaultCredentialReloadInterval is how often RunCredentialReloadLoop
// re-reads every configured deployment's *File credential source(s)
// when controlplane.CredentialReloadConfig.IntervalSeconds is unset
// (<= 0) — the same order of magnitude as this codebase's other
// periodic loops (e.g. a typical health_probe.interval_seconds).
const DefaultCredentialReloadInterval = 60 * time.Second

// RunCredentialReloadLoop periodically re-reads, from disk, the
// file-based credential source of every deployment that configured at
// least one DeploymentCredentialFiles field, atomically swapping that
// deployment's shared CredentialState whenever a freshly-read value
// differs from what's already loaded — see this file's own top-of-file
// design note for why a real file re-read (and NOT a bare os.Getenv
// re-read) is what actually lets a Kubernetes projected Secret volume
// rotation, or an operator's own rotation script rewriting the file,
// reach a running gateway process without a restart.
//
// A no-op (returns immediately) if interval <= 0, or if no configured
// deployment opted into file-based credentials at all — mirrors
// RunHealthProbeLoop's own "no-op unless configured" contract exactly,
// same ctx-canceled-by-SIGTERM/SIGINT wiring via cmd/gateway's run(),
// no separate stop mechanism needed.
func (p *Pipeline) RunCredentialReloadLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	// p.deploymentsByName is populated once, in NewPipeline, and never
	// mutated afterward (see that field's own doc comment) — safe to
	// range over here with no additional locking, exactly like
	// ProbeDeployments already does.
	reloadable := make([]Deployment, 0, len(p.deploymentsByName))
	for _, dep := range p.deploymentsByName {
		if dep.CredentialFiles.HasAny() && dep.CredentialState != nil {
			reloadable = append(reloadable, dep)
		}
	}
	if len(reloadable) == 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, dep := range reloadable {
				reloadDeploymentCredentials(dep, p.logger)
			}
		}
	}
}

// reloadDeploymentCredentials re-reads every file dep.CredentialFiles
// names, compares each freshly-read value against dep.CredentialState's
// current snapshot, and atomically publishes ONE new snapshot if any
// value actually changed — never on every tick regardless, so an
// unmounted/unchanged file re-reading the same bytes every interval
// stays a cheap, side-effect-free no-op. Logs a structured
// "which deployment, which field changed" event on a real rotation —
// NEVER the credential value itself. A read error (file missing,
// permission denied, a transient volume-mount hiccup) is logged as a
// warning and otherwise ignored: the deployment keeps using its
// last-known-good snapshot rather than ever swapping to an empty or
// partial value, so a bad rotation attempt degrades to "credentials go
// stale" rather than "credentials go dark".
func reloadDeploymentCredentials(dep Deployment, logger *slog.Logger) {
	prev := dep.CredentialState.Load()
	if prev == nil {
		// Unreachable in practice — NewDeploymentCredentialState always
		// seeds a non-nil value before a Deployment with CredentialFiles
		// set is ever published into p.deploymentsByName. Guarded anyway
		// so a future caller that constructs CredentialState differently
		// can't panic here.
		return
	}
	next := *prev
	changed := false

	reread := func(fieldName, path string, dst *string) {
		if path == "" {
			return
		}
		value, err := ReadCredentialFile(path)
		if err != nil {
			logger.Warn("credential_reload_read_failed",
				"deployment", dep.Name, "field", fieldName, "path", path, "error", err.Error())
			return
		}
		if value != *dst {
			*dst = value
			changed = true
			logger.Info("credential_reload_rotated", "deployment", dep.Name, "field", fieldName)
		}
	}

	reread("api_key", dep.CredentialFiles.APIKey, &next.APIKey)
	reread("access_key_id", dep.CredentialFiles.AccessKeyID, &next.AccessKeyID)
	reread("secret_access_key", dep.CredentialFiles.SecretAccessKey, &next.SecretAccessKey)
	reread("session_token", dep.CredentialFiles.SessionToken, &next.SessionToken)

	if changed {
		dep.CredentialState.Store(&next)
	}
}
