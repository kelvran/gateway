package compat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
)

// bedrockThinkingRejection is the body shape Bedrock's Converse returns for a
// parameter it rejects: a bare {"message"} with the exception named only in
// the x-amzn-ErrorType header -- not an Anthropic error envelope, which is
// exactly why RFC-1 §9's verbatim exception must not apply to it.
const bedrockThinkingRejection = `{"message":"The model returned the following errors: thinking.type: Input should be 'adaptive' or 'disabled'"}`

const bedrockErrorTypeHeader = "ValidationException:http://internal.amazon.com/coral/com.amazon.bedrock/"

// bedrockUpstream mocks Converse. It rejects any request that carries a
// thinking field with a scripted ValidationException -- whatever the model
// id, because the question under test is what the gateway does with such a
// rejection, while the capability table decides only whether the field is
// forwarded at all -- and answers everything else with the recorded golden.
type bedrockUpstream struct {
	recorder
	srv    *httptest.Server
	golden []byte
}

func newBedrockUpstream() (*bedrockUpstream, error) {
	golden, err := os.ReadFile(filepath.Join("testdata", "bedrock_converse_response.json"))
	if err != nil {
		return nil, fmt.Errorf("loading golden: %w", err)
	}
	u := &bedrockUpstream{golden: golden}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	return u, nil
}

func (u *bedrockUpstream) URL() string { return u.srv.URL }
func (u *bedrockUpstream) Close()      { u.srv.Close() }

func (u *bedrockUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "mock: reading body", http.StatusInternalServerError)
		return
	}
	u.record(r, body)
	if !strings.HasSuffix(r.URL.Path, "/converse") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		AdditionalModelRequestFields map[string]json.RawMessage `json:"additionalModelRequestFields"`
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"mock: unparsable body"}`)
		return
	}
	if _, ok := req.AdditionalModelRequestFields["thinking"]; ok {
		// What Bedrock stamps on a rejection, plus a header only an anthropic
		// deployment may relay -- the client must see none of them.
		w.Header().Set("x-amzn-ErrorType", bedrockErrorTypeHeader)
		w.Header().Set("x-amzn-RequestId", "mock-amzn-request-id")
		w.Header().Set("x-should-retry", "true")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, bedrockThinkingRejection)
		return
	}
	_, _ = w.Write(u.golden)
}
