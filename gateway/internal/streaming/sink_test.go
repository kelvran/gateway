package streaming

import "testing"

// *Writer is the OpenAI-format ChunkSink; the compile-time assertion is the
// contract every dataplane stream path relies on.
var _ ChunkSink = (*Writer)(nil)

// TestWriterIsAChunkSinkButNotAFinisher: the OpenAI writer's bytes are
// pinned (TestWriteChunkGoldenWireFormat) and its stream ends with [DONE]
// alone, so it must never pick up the Finisher extension by accident -- the
// dataplane would then route the end-of-stream facts into it instead of
// calling WriteDone.
func TestWriterIsAChunkSinkButNotAFinisher(t *testing.T) {
	sw, err := NewWriter(newFlushCountingRecorder())
	if err != nil {
		t.Fatal(err)
	}
	var sink ChunkSink = sw
	if _, ok := sink.(Finisher); ok {
		t.Fatal("*Writer implements Finisher; the [DONE] sentinel must stay the only end-of-stream write on the OpenAI route")
	}
	if (StreamEnd{Truncated: true}).Usage.TotalTokens != 0 {
		t.Fatal("StreamEnd zero value must carry zero usage")
	}
}
