package main

import (
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/admin"
)

// adminStatusTable: per admin operationId, the response statuses the handler
// can produce besides 401 (admin-api.md's Errors column, verified against the
// handlers: every JSON answer but GET /admin/config (its own encode-or-500) and
// GET /admin/audit (encode error discarded) goes through writeJSONResponse's
// encode-or-500; the pprof rows follow net/http/pprof under the pinned toolchain).
var adminStatusTable = map[string][]string{
	"getAdminConfig": {"200", "500"}, "queryAuditLog": {"200", "400", "500"}, "listVirtualKeys": {"200", "400", "500"},
	"upsertVirtualKey": {"204", "400", "413"}, "deleteVirtualKey": {"204", "400", "404", "409", "500"}, "rotateVirtualKey": {"204", "400", "404", "409", "413"},
	"getVirtualKeySpend": {"200", "404", "500"}, "getVirtualKeyInFlight": {"200", "404", "500"}, "listPrompts": {"200", "500"}, "getPrompt": {"200", "404", "500"},
	"getPromptVersion": {"200", "400", "404", "500"}, "upsertPrompt": {"200", "400", "413", "500"}, "deletePrompt": {"204", "400", "404", "500"},
	"setPromptLabel": {"200", "400", "404", "413", "500"}, "deletePromptLabel": {"204", "404", "500"}, "createBackup": {"200", "500", "501"},
	"listDeployments": {"200", "500"}, "updateDeploymentWeight": {"204", "400", "404", "413", "500"}, "eraseCacheEntry": {"200", "400", "404", "413", "500"},
	// The spend ledger's routes (item 13d slice 4): the read's 401 includes the cost-viewer-without-key= case (G27).
	"getSpend": {"200", "400", "500"}, "eraseSpendSession": {"200", "400", "413", "500"},
	// net/http/pprof's own, under the pinned toolchain: the index, cmdline and symbol never fail; Profile and Trace answer 500
	// when profiling cannot start and, since 2026-10-12, the gateway's own 400 above the 60 s `seconds` cap (internal/admin's
	// capPprofSeconds); the six named profiles answer 400/408/500 around a delta profile (`seconds`), the cap's 400 among them.
	// The seven write operations answer 413 over decodeJSONBody's bound (1 MiB; 4 KiB on the erase route).
	"pprofIndex": {"200"}, "pprofCmdline": {"200"}, "pprofProfile": {"200", "400", "500"}, "pprofSymbol": {"200"}, "pprofSymbolPost": {"200"}, "pprofTrace": {"200", "400", "500"},
	"pprofGoroutine": {"200", "400", "408", "500"}, "pprofHeap": {"200", "400", "408", "500"}, "pprofThreadcreate": {"200", "400", "408", "500"},
	"pprofBlock": {"200", "400", "408", "500"}, "pprofMutex": {"200", "400", "408", "500"}, "pprofAllocs": {"200", "400", "408", "500"},
}

// TestOpenAPIAdminStatuses: every admin operation declares exactly its table's
// statuses plus 401; every error response there is plain text; the backup
// route carries x-kelvran-when-unconfigured naming admin.backup_dir and 501.
func TestOpenAPIAdminStatuses(t *testing.T) {
	root := loadOpenAPI(t)
	byPattern := map[string]admin.Route{}
	ids := map[string]bool{}
	for _, rt := range admin.AllRoutes() {
		byPattern[rt.Pattern()] = rt
		ids[rt.OperationID] = true
	}
	for id := range adminStatusTable {
		if !ids[id] {
			t.Errorf("adminStatusTable names %q, which no admin route carries", id)
		}
	}
	checked := 0
	for _, op := range documentOperations(t, root) {
		rt, ok := byPattern[op.key()]
		if !ok {
			continue
		}
		checked++
		want := adminStatusTable[rt.OperationID]
		if want == nil {
			t.Errorf("%s: no row in adminStatusTable", rt.OperationID)
			continue
		}
		want = append(sortedCopy(want), "401")
		got := mappingKeys(mappingGet(op.node, "responses"))
		if strings.Join(sortedCopy(got), ",") != strings.Join(sortedCopy(want), ",") {
			t.Errorf("%s (yaml:%d): responses %v, want %v", op.key(), op.line, sortedCopy(got), sortedCopy(want))
		}
		responses := mappingGet(op.node, "responses")
		for _, status := range got {
			if status[0] == '2' {
				continue
			}
			content := mappingGet(mappingGet(responses, status), "content")
			if content == nil || mappingGet(content, "text/plain") == nil || len(mappingKeys(content)) != 1 {
				t.Errorf("%s %s: admin errors are plain text only", op.key(), status)
			}
		}
		if rt.OperationID == "createBackup" {
			var when struct {
				Status int    `yaml:"status"`
				Config string `yaml:"config"`
			}
			if n := mappingGet(op.node, "x-kelvran-when-unconfigured"); n == nil {
				t.Error("createBackup: x-kelvran-when-unconfigured missing")
			} else if err := n.Decode(&when); err != nil || when.Status != 501 || when.Config != "admin.backup_dir" {
				t.Errorf("createBackup: x-kelvran-when-unconfigured = %+v (%v), want 501 / admin.backup_dir", when, err)
			}
		}
	}
	if checked < 25 {
		t.Errorf("only %d admin operations checked; the loop lost the document", checked)
	}
}
