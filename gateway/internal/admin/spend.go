package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
)

// The spend ledger's two admin routes (RFC-2 item 13d, slice 4): GET
// /admin/spend aggregates the ledger by key, model, tool or session over a
// window; POST /admin/spend/erase removes one session's rows. Owner gates:
// G27 -- the cost-viewer tier reads only with key= naming one key, across
// every group_by (the first tier rule decided inside a handler rather than
// by the route wrapper); G28 -- the erase is Admin-only and its audit line
// carries a 16-hex SHA-256 prefix of the session, never the id, because the
// audit log is append-only and indefinite.

const (
	// spendLedgerTimeout bounds one ledger read or erase: in Redis mode a
	// read is one SCAN plus chunked HGETALLs over every hour hash in the
	// window, an erase every hour hash in the store.
	spendLedgerTimeout = 10 * time.Second
	// spendDefaultWindow is the window served when since/until are absent.
	spendDefaultWindow = 24 * time.Hour
	// spendEraseBodyLimit bounds POST /admin/spend/erase's body; the only
	// field is a 128-byte identifier.
	spendEraseBodyLimit = 4 << 10
	// costViewerTier is the tier name requireAnyBearerToken stashes for the
	// cost-viewer credential.
	costViewerTier = "cost_viewer"
)

// ledgerRetention is the configured retention, the floor of every window;
// a hand-built Config with no bounds (tests) gets the default.
func ledgerRetention(cfg *controlplane.Config) time.Duration {
	hours := cfg.Ledger.RetentionHours
	if hours < 1 {
		hours = controlplane.DefaultLedgerRetentionHours
	}
	return time.Duration(hours) * time.Hour
}

// spendWindow parses since/until (RFC 3339, each optional; defaults: the
// last spendDefaultWindow ending now), requires until after since, and clamps
// both to now - retention: the ledger holds nothing older, and the clamp
// makes the served window honest about it. A window that lies entirely
// before the floor collapses to [floor, floor) -- served empty, never
// inverted, never an error: the caller asked a valid question whose answer
// is that nothing is retained there.
func spendWindow(sinceRaw, untilRaw string, now time.Time, retention time.Duration) (since, until time.Time, err error) {
	until = now
	if untilRaw != "" {
		if until, err = time.Parse(time.RFC3339, untilRaw); err != nil {
			return since, until, fmt.Errorf("until %q is not a valid RFC 3339 timestamp: %w", untilRaw, err)
		}
	}
	since = until.Add(-spendDefaultWindow)
	if sinceRaw != "" {
		if since, err = time.Parse(time.RFC3339, sinceRaw); err != nil {
			return since, until, fmt.Errorf("since %q is not a valid RFC 3339 timestamp: %w", sinceRaw, err)
		}
	}
	if !until.After(since) {
		return since, until, errors.New("until must be after since")
	}
	if floor := now.Add(-retention); since.Before(floor) {
		since = floor
		if until.Before(floor) {
			until = floor
		}
	}
	return since.UTC(), until.UTC(), nil
}

// getSpendHandler serves GET /admin/spend?group_by=key|model|tool|session&key=&since=&until=.
// The wrapper admits Admin, Viewer and CostViewer; the handler applies G27.
// The audit line names the query and the row count, never a figure.
func getSpendHandler(pipeline *dataplane.Pipeline, retention time.Duration, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		groupBy := q.Get("group_by")
		if err := spendledger.ValidateGroupBy(groupBy); err != nil {
			http.Error(w, "group_by must be one of key, model, tool, session", http.StatusBadRequest)
			return
		}
		key := q.Get("key")
		if credentialTierFromContext(r.Context()) == costViewerTier && key == "" {
			// G27: a cost viewer sees one key at a time, never a cross-key
			// aggregate; an insufficient tier is 401 on this listener, never 403.
			http.Error(w, "cost_viewer may read /admin/spend only with key= naming one virtual key", http.StatusUnauthorized)
			return
		}
		since, until, err := spendWindow(q.Get("since"), q.Get("until"), time.Now(), retention)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), spendLedgerTimeout)
		defer cancel()
		rows, err := pipeline.QuerySpend(ctx, spendledger.Query{GroupBy: groupBy, Key: key, Since: since, Until: until})
		if err != nil {
			// The ledger's error strings name hours and keys, never a row or a
			// session (every backend's contract since slices 1-2, pinned by
			// their non-leak tests), so the text can go to the caller.
			http.Error(w, fmt.Sprintf("querying spend ledger: %v", err), http.StatusInternalServerError)
			return
		}
		resp := adminapi.SpendResponse{GroupBy: groupBy, Key: key, Since: since, Until: until, Rows: make([]adminapi.SpendRow, 0, len(rows))}
		for _, row := range rows {
			resp.Rows = append(resp.Rows, adminapi.SpendRow{Key: row.Key, Model: row.Model, Tool: row.Tool, Session: row.Session, SpendUSD: row.SpendUSD.String(), Requests: row.Requests})
		}
		writeJSONResponse(w, resp)
		logger.Info("admin_spend_read", "group_by", groupBy, "key", key, "since", since.Format(time.RFC3339), "until", until.Format(time.RFC3339), "rows", len(resp.Rows), "authorized_by", credentialTierFromContext(r.Context()))
	}
}

// eraseSpendSessionHandler serves POST /admin/spend/erase with body
// {"session": "<id>"}: Admin-only by its wrapper; 400 for a body that is not
// JSON, an empty session, the fold target `other` (removed only with its key
// or by retention) or a value outside the identifier grammar. The audit line
// carries a 16-hex SHA-256 prefix of the session and the count (G28).
func eraseSpendSessionHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req adminapi.SpendEraseRequest
		// encoding/json's errors carry an offset or a type name, never the
		// body's bytes, and the body is bounded, so the text can go back.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, spendEraseBodyLimit)).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		switch {
		case req.Session == "":
			http.Error(w, "session is required", http.StatusBadRequest)
			return
		case req.Session == spendledger.OtherSession:
			http.Error(w, "session \"other\" cannot be erased on its own: it is the fold target, removed only with its key or by retention", http.StatusBadRequest)
			return
		case !spendledger.IsSessionIdentifier(req.Session):
			http.Error(w, fmt.Sprintf("session must be 1-%d bytes of [A-Za-z0-9._:-]", spendledger.MaxSessionBytes), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), spendLedgerTimeout)
		defer cancel()
		n, err := pipeline.EraseSpendSession(ctx, req.Session)
		if err != nil {
			http.Error(w, fmt.Sprintf("erasing spend ledger session: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, adminapi.SpendEraseResponse{RowsErased: n})
		logger.Info("admin_spend_erased", "session_sha256_prefix", sessionAuditPrefix(req.Session), "rows_erased", n, "authorized_by", credentialTierFromContext(r.Context()))
	}
}

// sessionAuditPrefix is the first 16 hex characters of the session's SHA-256:
// enough to correlate an erasure with a request, never the identifier itself.
func sessionAuditPrefix(session string) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:])[:16]
}
