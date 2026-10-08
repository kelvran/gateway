package bench

import (
	"io"
	"testing"
	"time"
)

// TestReadSSETimesTTFTOnTheFirstContentFrameNotTheRoleFrame: a parser that
// stamped TTFT at the first data frame (the role-only chunk) would pass the
// other tests; this one separates the two by 30 ms.
func TestReadSSETimesTTFTOnTheFirstContentFrameNotTheRoleFrame(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = io.WriteString(pw, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(pw, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(pw, "data: [DONE]\n\n")
	}()
	timing, err := ReadSSE(pr, time.Now())
	if err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if timing.TTFT < 30*time.Millisecond {
		t.Errorf("TTFT = %s, want >= 30ms (the first CONTENT frame, not the role frame)", timing.TTFT)
	}
	if len(timing.ContentGaps) != 0 || len(timing.Gaps) != 1 {
		t.Errorf("content gaps/gaps = %d/%d, want 0/1", len(timing.ContentGaps), len(timing.Gaps))
	}
}
