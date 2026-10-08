package bench

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// StreamTiming is what ReadSSE measures about one server-sent-events body.
type StreamTiming struct {
	// TTFT is the time from start to the first frame that carries content
	// (the first token a user sees); when no frame carries content it is the
	// time to the first data frame.
	TTFT time.Duration
	// Total is the time from start to the end of the body.
	Total time.Duration
	// Frames counts data frames other than the [DONE] sentinel.
	Frames int
	// Gaps are the intervals between consecutive data frames; ContentGaps
	// only between consecutive frames that carry content (the inter-token
	// cadence a user perceives).
	Gaps        []time.Duration
	ContentGaps []time.Duration
	// Done reports the [DONE] sentinel; ErrorFrame an in-band
	// {"error": …} frame (the gateway's mid-stream failure shape).
	Done       bool
	ErrorFrame bool
}

// ReadSSE consumes r to EOF, timing every "data:" frame relative to start.
func ReadSSE(r io.Reader, start time.Time) (StreamTiming, error) {
	var t StreamTiming
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var lastFrame, lastContent time.Time
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		now := time.Now()
		if payload == "[DONE]" {
			t.Done = true
			continue
		}
		t.Frames++
		if !lastFrame.IsZero() {
			t.Gaps = append(t.Gaps, now.Sub(lastFrame))
		}
		lastFrame = now
		if strings.HasPrefix(payload, `{"error"`) {
			t.ErrorFrame = true
		}
		if frameHasContent(payload) {
			if lastContent.IsZero() {
				t.TTFT = now.Sub(start)
			} else {
				t.ContentGaps = append(t.ContentGaps, now.Sub(lastContent))
			}
			lastContent = now
		} else if t.Frames == 1 {
			t.TTFT = now.Sub(start) // provisional: the first content frame overrides it
		}
	}
	t.Total = time.Since(start)
	if err := sc.Err(); err != nil {
		return t, err
	}
	return t, nil
}

type contentProbe struct {
	Choices []struct {
		Delta struct {
			Content *string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

func frameHasContent(payload string) bool {
	var p contentProbe
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false
	}
	return len(p.Choices) > 0 && p.Choices[0].Delta.Content != nil && *p.Choices[0].Delta.Content != ""
}
