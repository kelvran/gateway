package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

// GET /v1/models — one SUPERSET document for every client that lists models.
//
// Three clients read this route with three different shapes: the OpenAI SDK
// reads `object: "list"` and `data[].id|object|created|owned_by`; the
// Anthropic SDK reads `data[].id|type|display_name|created_at` plus
// `first_id`/`last_id`/`has_more` and auto-paginates on them; Claude Code's
// provider discovery reads `data[].id|display_name|description`. Every one
// of them ignores fields it does not know, two routes on one path are
// impossible, and sniffing headers to pick a dialect would be a second
// contract to keep -- so one document carries all three.
//
// The pattern is registered as the EXACT path "/v1/models" (no trailing
// slash), so "/v1/models/" is a 404, never a ServeMux 301: Claude Code
// treats any redirect from this route as a failed provider.
//
// `limit` defaults to "everything" (the OpenAI SDK reads exactly one page;
// Anthropic's default of 20 would silently drop models) and is clamped to
// modelsListMaxLimit; a non-integer or non-positive value is a 400
// envelope naming `limit`. `after_id`/`before_id` walk the id-sorted list
// so Anthropic's auto-pagination terminates.
const modelsListMaxLimit = 1000

type modelsListDocument struct {
	Object  string       `json:"object"`
	Data    []modelEntry `json:"data"`
	FirstID *string      `json:"first_id"`
	LastID  *string      `json:"last_id"`
	HasMore bool         `json:"has_more"`
}

// modelEntry is one model in both dialects at once.
type modelEntry struct {
	ID          string `json:"id"`
	Object      string `json:"object"`       // OpenAI: "model"
	Created     int64  `json:"created"`      // OpenAI: Unix seconds (catalog load time)
	OwnedBy     string `json:"owned_by"`     // OpenAI: the serving providers, comma-joined
	Type        string `json:"type"`         // Anthropic: "model"
	CreatedAt   string `json:"created_at"`   // Anthropic: RFC 3339 (catalog load time)
	DisplayName string `json:"display_name"` // Anthropic / Claude Code; falls back to id
	Description string `json:"description"`  // Claude Code; "" when the operator set none
	Kind        string `json:"kind"`         // Kelvran: "chat" | "embedding"
}

func modelsHandler(p *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		query := r.URL.Query()
		limit, ok := parseModelsLimit(query.Get("limit"))
		if !ok {
			invalidRequest(w, http.StatusBadRequest, "invalid_request", codePtr("limit"), "limit must be a positive integer")
			return
		}
		afterID, beforeID := query.Get("after_id"), query.Get("before_id")
		if afterID != "" && beforeID != "" {
			invalidRequest(w, http.StatusBadRequest, "invalid_request", codePtr("after_id"), "after_id and before_id cannot be combined")
			return
		}

		models, err := p.HandleListModels(r.Context(), r.Header.Get("Authorization"), r.RemoteAddr)
		if err != nil {
			writeErrorResponse(w, err)
			return
		}
		page, hasMore, badCursor := paginateModels(models, limit, afterID, beforeID)
		if badCursor != "" {
			invalidRequest(w, http.StatusBadRequest, "invalid_request", codePtr(badCursor), badCursor+" does not name a model this key can see")
			return
		}

		created := p.CatalogLoadedAt()
		doc := modelsListDocument{Object: "list", Data: make([]modelEntry, 0, len(page)), HasMore: hasMore}
		for _, m := range page {
			doc.Data = append(doc.Data, modelEntryFrom(m, created))
		}
		if len(doc.Data) > 0 {
			first, last := doc.Data[0].ID, doc.Data[len(doc.Data)-1].ID
			doc.FirstID, doc.LastID = &first, &last
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := json.NewEncoder(w).Encode(doc); err != nil {
			slog.Error("encoding models list", "error", err)
		}
	}
}

func modelEntryFrom(m dataplane.ModelInfo, created time.Time) modelEntry {
	displayName := m.DisplayName
	if displayName == "" {
		displayName = m.ID
	}
	return modelEntry{
		ID:          m.ID,
		Object:      "model",
		Created:     created.Unix(),
		OwnedBy:     strings.Join(m.Providers, ","),
		Type:        "model",
		CreatedAt:   created.UTC().Format(time.RFC3339),
		DisplayName: displayName,
		Description: m.Description,
		Kind:        m.Kind,
	}
}

// parseModelsLimit: "" means every model (0); an integer >= 1 is accepted
// and clamped to modelsListMaxLimit; anything else is rejected.
func parseModelsLimit(raw string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, false
	}
	if n > modelsListMaxLimit {
		n = modelsListMaxLimit
	}
	return n, true
}

// paginateModels slices the id-sorted list. limit 0 means all. after_id
// yields the page that follows the cursor, before_id the page that
// precedes it (Anthropic's cursor semantics). badCursor names the query
// parameter whose id is not in the list the caller may see.
func paginateModels(models []dataplane.ModelInfo, limit int, afterID, beforeID string) (page []dataplane.ModelInfo, hasMore bool, badCursor string) {
	start, end := 0, len(models)
	if afterID != "" {
		idx := indexOfModel(models, afterID)
		if idx < 0 {
			return nil, false, "after_id"
		}
		start = idx + 1
	}
	if beforeID != "" {
		idx := indexOfModel(models, beforeID)
		if idx < 0 {
			return nil, false, "before_id"
		}
		end = idx
	}
	window := end - start
	if window < 0 {
		window = 0
	}
	if limit == 0 || limit > window {
		limit = window
	}
	if beforeID != "" {
		pageStart := end - limit
		return models[pageStart:end], pageStart > 0, ""
	}
	pageEnd := start + limit
	return models[start:pageEnd], pageEnd < end, ""
}

func indexOfModel(models []dataplane.ModelInfo, id string) int {
	for i, m := range models {
		if m.ID == id {
			return i
		}
	}
	return -1
}

// modelMetadataFromConfig converts the control plane's `models:` section
// into the dataplane's metadata map (nil when the section is absent).
func modelMetadataFromConfig(models map[string]controlplane.ModelMetadataConfig) map[string]dataplane.ModelMetadata {
	if len(models) == 0 {
		return nil
	}
	out := make(map[string]dataplane.ModelMetadata, len(models))
	for id, m := range models {
		out[id] = dataplane.ModelMetadata{DisplayName: m.DisplayName, Description: m.Description}
	}
	return out
}
