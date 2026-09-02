package streaming

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
