package embedsim

import (
	"context"
	"log/slog"
	"time"

	"github.com/kelvran/gateway/gateway/internal/credentialstate"
)

// RunCredentialReloadLoop periodically re-reads e's file-based
// credential source(s), atomically swapping e.credState whenever a
// freshly-read value differs -- see bedrockguard.Detector's identically
// named, identically-shaped method for the full rationale (both mirror
// dataplane.Pipeline.RunCredentialReloadLoop's own mechanism, built on
// the shared internal/credentialstate primitive).
//
// A no-op (returns immediately) if interval <= 0, or if e was never
// configured with any *File field at all.
func (e *BedrockEmbedder) RunCredentialReloadLoop(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 || !e.credFiles.HasAny() {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.reloadCredentials(logger)
		}
	}
}

// reloadCredentials re-reads every file e.credFiles names and
// atomically publishes a new snapshot if any value actually changed --
// see bedrockguard.Detector.reloadCredentials for the full rationale,
// including why no nil-check on the loaded pointer is needed here
// (NewBedrockEmbedder unconditionally seeds e.credState).
func (e *BedrockEmbedder) reloadCredentials(logger *slog.Logger) {
	prev := e.credState.Load()
	next := *prev
	changed := false

	reread := func(fieldName, path string, dst *string) {
		if path == "" {
			return
		}
		value, err := credentialstate.ReadFile(path)
		if err != nil {
			logger.Warn("embedsim_credential_reload_read_failed",
				"field", fieldName, "path", path, "error", err.Error())
			return
		}
		if value != *dst {
			*dst = value
			changed = true
			logger.Info("embedsim_credential_reload_rotated", "field", fieldName)
		}
	}

	reread("access_key_id", e.credFiles.AccessKeyID, &next.AccessKeyID)
	reread("secret_access_key", e.credFiles.SecretAccessKey, &next.SecretAccessKey)
	reread("session_token", e.credFiles.SessionToken, &next.SessionToken)

	if changed {
		e.credState.Store(&next)
	}
}

// credentialReloader is satisfied by any Embedder that owns its own
// atomically-swappable, file-based credential state -- true for
// *BedrockEmbedder, false for a test fake (e.g. embedsim_test.go's
// wordHashEmbedder). Defined here, not exported, since only Detector's
// own delegating RunCredentialReloadLoop below needs it.
type credentialReloader interface {
	RunCredentialReloadLoop(ctx context.Context, interval time.Duration, logger *slog.Logger)
}

// RunCredentialReloadLoop on *Detector delegates to d's own embedder if
// (and only if) it supports credential hot-reload -- a harmless no-op
// otherwise. This is what lets cmd/gateway's run() drive every
// guardrail.Detector's reload loop through one shared type assertion
// (against its own, package-local credentialReloader interface) without
// ever needing to know that embedsim.Detector wraps a separate embedder
// type internally at all.
func (d *Detector) RunCredentialReloadLoop(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if reloader, ok := d.embedder.(credentialReloader); ok {
		reloader.RunCredentialReloadLoop(ctx, interval, logger)
	}
}
