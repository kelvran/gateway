package admin

import (
	"net/http"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// Tier names the auth wrapper a route is registered with: exactly one value
// per wrapper in admin.go, so the table and the wrapper cannot disagree, and
// the OpenAPI document's security requirements derive from the table (plan
// item 14). Tiers returns the audit log's authorized_by names each wrapper admits.
type Tier string

const (
	// TierAdmin is requireBearerToken(creds.Admin, ...): every plain write
	// route and pprof (profiling data is a stronger information-disclosure and
	// DoS-surface signal than anything the viewer tier sees).
	TierAdmin Tier = "admin"
	// TierAdminOrViewer is requireEitherBearerToken: the read-only routes,
	// per docs/rfcs/2026-09-09-gateway-admin-viewer-role.md.
	TierAdminOrViewer Tier = "admin_or_viewer"
	// TierAdminOrOperator is requireAdminOrOperatorBearerToken: the three
	// reversible, single-target writes (rotate, reweight, erase), per
	// docs/rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md.
	TierAdminOrOperator Tier = "admin_or_operator"
	// TierAdminViewerCostViewer is requireAnyBearerToken over admin, viewer
	// and cost_viewer: the two spend reads the CostViewer tier authenticates,
	// GET /admin/virtual_keys/{name}/spend and GET /admin/spend (the latter
	// only with key= naming one key -- G27, decided inside its handler).
	TierAdminViewerCostViewer Tier = "admin_viewer_cost_viewer"
)

// Tiers returns the authorized_by names the wrapper admits, admin first; an
// unknown tier panics, like requireTier.
func (t Tier) Tiers() []string {
	switch t {
	case TierAdmin:
		return []string{"admin"}
	case TierAdminOrViewer:
		return []string{"admin", "viewer"}
	case TierAdminOrOperator:
		return []string{"admin", "operator"}
	case TierAdminViewerCostViewer:
		return []string{"admin", "viewer", "cost_viewer"}
	}
	panic("admin: unknown tier " + string(t))
}

// Condition names the config key that must be set for a route to be
// registered at all; Always registers unconditionally.
type Condition string

const (
	// Always registers the route unconditionally.
	Always Condition = ""
	// IfAuditLogPath: GET /admin/audit exists only when cmd/gateway opened an
	// audit store, which it does only for a configured admin.audit_log_path.
	IfAuditLogPath Condition = "admin.audit_log_path"
	// IfEnablePprof: the twelve pprof patterns, off by default.
	IfEnablePprof Condition = "admin.enable_pprof"
)

// Route is one row of the admin listener's route table -- the single source
// Handler registers from and the contract test compares the OpenAPI document
// against. Path uses Go 1.22 wildcards exactly as registered; the one path
// ending in "/" is the one subtree pattern.
type Route struct {
	// OperationID is the OpenAPI operationId and the key routeHandlers uses,
	// so no "METHOD /admin/..." literal exists outside this file.
	OperationID string
	Method      string // GET, POST, PUT or DELETE
	Path        string
	Tier        Tier
	Condition   Condition
}

// Pattern is the exact string handed to (*http.ServeMux).Handle.
func (r Route) Pattern() string { return r.Method + " " + r.Path }

// Subtree reports a trailing-slash pattern, which net/http matches as a
// prefix: unlisted sub-paths resolve to it and the slash-less form redirects.
func (r Route) Subtree() bool { return strings.HasSuffix(r.Path, "/") }

// pprofProfiles are the named profiles net/http/pprof registers on
// DefaultServeMux in its init; pprof.Handler(name) looks each up by name,
// the indirection its own docs recommend for mounting under a custom prefix.
var pprofProfiles = []string{"goroutine", "heap", "threadcreate", "block", "mutex", "allocs"}

// pprofOperationID is the operationId of a named profile's route: pprofHeap.
func pprofOperationID(profile string) string {
	return "pprof" + strings.ToUpper(profile[:1]) + profile[1:]
}

// routeTable holds every route in registration order: 20 unconditional, the
// audit route, the twelve pprof routes -- 33 rows over 28 paths. Prompts are
// global operator-managed config, so their reads are viewer-or-admin like
// GET /admin/config and their writes admin-only like the virtual-key routes;
// POST /admin/backup is always registered so an unconfigured caller gets an
// informative 501 from the handler rather than a 404 indistinguishable from
// a typo.
var routeTable = buildRouteTable()

func buildRouteTable() []Route {
	table := []Route{
		{"getAdminConfig", http.MethodGet, "/admin/config", TierAdminOrViewer, Always},
		{"queryAuditLog", http.MethodGet, "/admin/audit", TierAdminOrViewer, IfAuditLogPath},
		{"listVirtualKeys", http.MethodGet, "/admin/virtual_keys", TierAdminOrViewer, Always},
		{"upsertVirtualKey", http.MethodPost, "/admin/virtual_keys/{name}", TierAdmin, Always},
		{"deleteVirtualKey", http.MethodDelete, "/admin/virtual_keys/{name}", TierAdmin, Always},
		{"rotateVirtualKey", http.MethodPost, "/admin/virtual_keys/{name}/rotate", TierAdminOrOperator, Always},
		{"getVirtualKeySpend", http.MethodGet, "/admin/virtual_keys/{name}/spend", TierAdminViewerCostViewer, Always},
		// In-flight load is operational topology state, the category
		// GET /admin/virtual_keys exposes -- not a cost view, so no CostViewer.
		{"getVirtualKeyInFlight", http.MethodGet, "/admin/virtual_keys/{name}/inflight", TierAdminOrViewer, Always},
		// The spend ledger (RFC-2 item 13d, G27/G28): the read admits the
		// cost-viewer tier too, but only with key= -- decided inside the
		// handler; the erase is irreversible and cross-tenant, so Admin only.
		{"getSpend", http.MethodGet, "/admin/spend", TierAdminViewerCostViewer, Always},
		{"eraseSpendSession", http.MethodPost, "/admin/spend/erase", TierAdmin, Always},
		{"listPrompts", http.MethodGet, "/admin/prompts", TierAdminOrViewer, Always},
		{"getPrompt", http.MethodGet, "/admin/prompts/{id}", TierAdminOrViewer, Always},
		{"getPromptVersion", http.MethodGet, "/admin/prompts/{id}/versions/{version}", TierAdminOrViewer, Always},
		{"upsertPrompt", http.MethodPost, "/admin/prompts/{id}", TierAdmin, Always},
		{"deletePrompt", http.MethodDelete, "/admin/prompts/{id}", TierAdmin, Always},
		// Label promote/rollback reuse the Admin tier exactly; rollback is
		// SetLabel to an older version, not a separate route.
		{"setPromptLabel", http.MethodPut, "/admin/prompts/{id}/labels/{label}", TierAdmin, Always},
		{"deletePromptLabel", http.MethodDelete, "/admin/prompts/{id}/labels/{label}", TierAdmin, Always},
		{"createBackup", http.MethodPost, "/admin/backup", TierAdmin, Always},
		{"listDeployments", http.MethodGet, "/admin/deployments", TierAdminOrViewer, Always},
		{"updateDeploymentWeight", http.MethodPost, "/admin/deployments/{name}/weight", TierAdminOrOperator, Always},
		{"eraseCacheEntry", http.MethodPost, "/admin/cache/erase", TierAdminOrOperator, Always},
		{"pprofIndex", http.MethodGet, "/admin/debug/pprof/", TierAdmin, IfEnablePprof},
		{"pprofCmdline", http.MethodGet, "/admin/debug/pprof/cmdline", TierAdmin, IfEnablePprof},
		{"pprofProfile", http.MethodGet, "/admin/debug/pprof/profile", TierAdmin, IfEnablePprof},
		{"pprofSymbol", http.MethodGet, "/admin/debug/pprof/symbol", TierAdmin, IfEnablePprof},
		{"pprofSymbolPost", http.MethodPost, "/admin/debug/pprof/symbol", TierAdmin, IfEnablePprof},
		{"pprofTrace", http.MethodGet, "/admin/debug/pprof/trace", TierAdmin, IfEnablePprof},
	}
	for _, profile := range pprofProfiles {
		table = append(table, Route{pprofOperationID(profile), http.MethodGet, "/admin/debug/pprof/" + profile, TierAdmin, IfEnablePprof})
	}
	return table
}

// Routes returns the rows Handler registers for cfg and hasAuditStore, in
// registration order: 20 always, +1 with an audit store, +12 with
// cfg.Admin.EnablePprof. Pure.
func Routes(cfg *controlplane.Config, hasAuditStore bool) []Route {
	out := make([]Route, 0, len(routeTable))
	for _, rt := range routeTable {
		switch rt.Condition {
		case Always:
		case IfAuditLogPath:
			if !hasAuditStore {
				continue
			}
		case IfEnablePprof:
			if !cfg.Admin.EnablePprof {
				continue
			}
		default:
			panic("admin: route " + rt.Pattern() + " has an unknown condition " + string(rt.Condition))
		}
		out = append(out, rt)
	}
	return out
}

// AllRoutes returns every row with every condition on -- the set the OpenAPI
// document describes (33 rows over 28 paths). A copy; callers may sort it.
func AllRoutes() []Route {
	out := make([]Route, len(routeTable))
	copy(out, routeTable)
	return out
}
