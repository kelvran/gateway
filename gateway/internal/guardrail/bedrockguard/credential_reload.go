package bedrockguard

import (
	"context"
	"log/slog"
	"time"

	"github.com/kelvran/gateway/gateway/internal/credentialstate"
)

// RunCredentialReloadLoop periodically re-reads d's file-based
// credential source(s), atomically swapping d.credState whenever a
// freshly-read value differs -- the identical mechanism
// dataplane.Pipeline.RunCredentialReloadLoop already provides for
// Deployment credentials, applied here so an AWS STS session token (or
// a Kubernetes projected Secret volume rotation) reaches a running
// Bedrock Guardrails call without a gateway restart. See
// internal/credentialstate's own package doc comment for why the
// shared primitive lives there and not in dataplane.
//
// A no-op (returns immediately) if interval <= 0, or if d was never
// configured with any *File field at all -- mirrors
// dataplane.Pipeline.RunCredentialReloadLoop's identical "opt-in, no-op
// otherwise" contract. Started unconditionally by cmd/gateway's run()
// via a type assertion against credentialReloader, scoped to the same
// ctx as every other background loop (SIGTERM/SIGINT-canceled).
func (d *Detector) RunCredentialReloadLoop(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 || !d.credFiles.HasAny() {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.reloadCredentials(logger)
		}
	}
}

// reloadCredentials re-reads every file d.credFiles names, compares
// each freshly-read value against d.credState's current snapshot, and
// atomically publishes ONE new snapshot if any value actually changed
// -- never on every tick regardless. Logs a structured "which field
// changed" event on a real rotation, NEVER the credential value itself.
// A read error (file missing, permission denied, a transient
// volume-mount hiccup) is logged as a warning and otherwise ignored:
// the Detector keeps using its last-known-good snapshot rather than
// ever swapping to an empty or partial value.
//
// No nil-check on the loaded pointer, unlike dataplane's own
// reloadDeploymentCredentials (which guards a genuinely-nilable
// Deployment.CredentialState with a defensive, "unreachable in
// practice" check) -- New above unconditionally seeds d.credState via
// credentialstate.NewState(...) regardless of whether any *File field
// is even set, so it can never be nil here.
func (d *Detector) reloadCredentials(logger *slog.Logger) {
	prev := d.credState.Load()
	next := *prev
	changed := false

	reread := func(fieldName, path string, dst *string) {
		if path == "" {
			return
		}
		value, err := credentialstate.ReadFile(path)
		if err != nil {
			logger.Warn("bedrockguard_credential_reload_read_failed",
				"field", fieldName, "path", path, "error", err.Error())
			return
		}
		if value != *dst {
			*dst = value
			changed = true
			logger.Info("bedrockguard_credential_reload_rotated", "field", fieldName)
		}
	}

	reread("access_key_id", d.credFiles.AccessKeyID, &next.AccessKeyID)
	reread("secret_access_key", d.credFiles.SecretAccessKey, &next.SecretAccessKey)
	reread("session_token", d.credFiles.SessionToken, &next.SessionToken)

	if changed {
		d.credState.Store(&next)
	}
}
