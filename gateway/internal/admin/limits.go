package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// adminWriteBodyLimit bounds the JSON body of every admin write route but
// POST /admin/spend/erase (spendEraseBodyLimit, 4 KiB, whose body has one
// member). A prompt template is the largest legitimate body and 1 MiB is far
// beyond any model's context; the data plane caps its own bodies at 32 MiB
// (cmd/gateway's maxRequestBodyBytes), and the admin listener -- loopback by
// default, bearer-protected -- has no reason to accept anything near that.
// Before 2026-10-12 the six routes read the body unbounded.
const adminWriteBodyLimit int64 = 1 << 20

// pprofMaxSeconds caps the `seconds` query parameter of the nine pprof routes
// that read it: profile, trace and the seven delta profiles. Under the pinned
// toolchain net/http/pprof extends the connection's write deadline by
// WriteTimeout + seconds and never bounds seconds itself, so an Admin-tier
// caller could hold the exclusive CPU profiler and a connection for as long as
// it asked. 60 is twice the CPU profile's default of 30 and keeps the
// deadline extension at most 120 s.
const pprofMaxSeconds = 60

// decodeJSONBody decodes r's JSON body into dst under limit bytes. Over the
// limit it answers 413 `request body too large`; on any other decode error the
// 400 `invalid request body: …` every write route has always answered
// (encoding/json's errors carry an offset or a type name, never the body's
// bytes, so the text can go back). It returns false once it has written a
// response.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// capPprofSeconds answers 400 when the request's `seconds` parses to a value
// above maxSeconds and otherwise hands the request to next untouched, so
// net/http/pprof keeps its own defaults (an absent, unparsable or non-positive
// value) and its own 400s (a delta profile's positive-integer rule).
func capPprofSeconds(next http.Handler, maxSeconds float64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.URL.Query().Get("seconds"); raw != "" {
			if sec, err := strconv.ParseFloat(raw, 64); err == nil && sec > maxSeconds {
				http.Error(w, "seconds must be at most "+strconv.FormatFloat(maxSeconds, 'f', -1, 64), http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
