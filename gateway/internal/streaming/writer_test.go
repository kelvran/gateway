package streaming

import (
	"net/http"
	"net/http/httptest"
	"strings"

	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// flushCountingRecorder wraps httptest.ResponseRecorder to additionally
// count Flush calls, so tests can assert WriteChunk/WriteDone flush exactly
// once per call rather than merely writing bytes.
type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCountingRecorder) Flush() {
	f.flushes++
}

func newFlushCountingRecorder() *flushCountingRecorder {
	return &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func TestNewWriterRejectsNonFlushableResponseWriter(t *testing.T) {
	// httptest.ResponseRecorder implements http.Flusher itself, so to
	// exercise the rejection path we wrap it in something that doesn't.
	type nonFlushable struct{ http.ResponseWriter }
	_, err := NewWriter(nonFlushable{httptest.NewRecorder()})
	if err == nil {
		t.Fatal("NewWriter() error = nil, want an error for a non-flushing ResponseWriter")
	}
}

func TestWriteChunkWritesAndFlushes(t *testing.T) {
	rec := newFlushCountingRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}

	chunk := ChatCompletionChunk{
		ID:    "chunk-1",
		Model: "gpt-4o",
		Choices: []ChunkChoice{
			{Index: 0, Delta: MessageDelta{Content: "hi"}},
		},
	}
	if err := sw.WriteChunk(chunk); err != nil {
		t.Fatalf("WriteChunk() error = %v", err)
	}

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("body = %q, want prefix %q", body, "data: ")
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("body = %q, want suffix %q", body, "\\n\\n")
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("body = %q, want it to contain the chunk's JSON encoding", body)
	}
	if rec.flushes != 1 {
		t.Errorf("flushes = %d, want exactly 1", rec.flushes)
	}
}

func TestWriteDoneWritesSentinelAndFlushes(t *testing.T) {
	rec := newFlushCountingRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}

	if err := sw.WriteDone(); err != nil {
		t.Fatalf("WriteDone() error = %v", err)
	}
	if rec.Body.String() != "data: [DONE]\n\n" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "data: [DONE]\n\n")
	}
	if rec.flushes != 1 {
		t.Errorf("flushes = %d, want exactly 1", rec.flushes)
	}
}

func TestWriteChunkMultipleCallsEachFlushOnce(t *testing.T) {
	rec := newFlushCountingRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := sw.WriteChunk(ChatCompletionChunk{ID: "c"}); err != nil {
			t.Fatalf("WriteChunk() error = %v", err)
		}
	}
	if rec.flushes != 3 {
		t.Errorf("flushes = %d, want 3 (one per WriteChunk call)", rec.flushes)
	}
}

// TestWriteChunkEncodesNilChoicesAsEmptyArray: the usage-only final chunk is
// built without choices; OpenAI's wire format carries "choices": [] there,
// and clients that validate the shape (LlamaIndex's stream_chat does
// len(choices); the Vercel AI SDK's schema requires an array) fail on null.
func TestWriteChunkEncodesNilChoicesAsEmptyArray(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	usage := 7
	if err := w.WriteChunk(ChatCompletionChunk{ID: "chatcmpl-x", Object: "chat.completion.chunk", Model: "m", Usage: &adapter.Usage{TotalTokens: usage}}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"choices":[]`) || strings.Contains(body, `"choices":null`) {
		t.Fatalf("usage-only chunk must carry \"choices\":[] on the wire, got %s", body)
	}
}
