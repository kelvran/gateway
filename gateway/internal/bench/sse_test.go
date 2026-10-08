package bench

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestReadSSECountsFramesAndDetectsTheSentinel(t *testing.T) {
	body := "data: {\"id\":\"a\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"a\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		": a comment the parser must ignore\n\n" +
		"data: {\"id\":\"a\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	timing, err := ReadSSE(strings.NewReader(body), time.Now())
	if err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if timing.Frames != 3 || !timing.Done || timing.ErrorFrame {
		t.Errorf("timing = %+v, want 3 data frames, done, no error frame", timing)
	}
	if timing.TTFT <= 0 {
		t.Errorf("TTFT = %s, want a positive duration measured from the start", timing.TTFT)
	}
	if len(timing.Gaps) != 2 {
		t.Errorf("gaps = %d, want 2 (between three data frames)", len(timing.Gaps))
	}
}

func TestReadSSEFlagsAnInBandErrorFrameAndAMissingSentinel(t *testing.T) {
	body := "data: {\"id\":\"a\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"upstream call failed\",\"type\":\"server_error\",\"code\":\"upstream_error\"}}\n\n"
	timing, err := ReadSSE(strings.NewReader(body), time.Now())
	if err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if !timing.ErrorFrame || timing.Done {
		t.Errorf("timing = %+v, want the in-band error frame flagged and no [DONE]", timing)
	}
}

func TestReadSSEMeasuresInterFrameGaps(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = io.WriteString(pw, "data: {\"a\":1}\n\n")
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(pw, "data: {\"a\":2}\n\n")
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(pw, "data: [DONE]\n\n")
	}()
	timing, err := ReadSSE(pr, time.Now())
	if err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if len(timing.Gaps) != 1 || timing.Gaps[0] < 25*time.Millisecond {
		t.Errorf("gaps = %v, want one gap of at least ~30ms", timing.Gaps)
	}
	if timing.Total < 55*time.Millisecond {
		t.Errorf("total = %s, want at least ~60ms", timing.Total)
	}
}
