package translate

import (
	"io"
	"strings"
	"testing"
	"time"
)

func nilEOF() error { return io.EOF }

// TestStreamWriterEmptyUpstreamFails covers a 200 from upstream that carries
// no events at all: Consume must report it as a failure — the proxy retries
// such streams — instead of emitting a fake complete message that hides the
// empty turn.
func TestStreamWriterEmptyUpstreamFails(t *testing.T) {
	var output strings.Builder
	writer := NewStreamWriter(&output, nil, "m", false)
	if err := writer.Consume(strings.NewReader("")); err == nil {
		t.Fatal("an empty upstream stream must be reported as a failure")
	}
	if output.String() != "" {
		t.Fatalf("no events may be emitted for an empty stream:\n%s", output.String())
	}
}

// TestStreamWriterKeepalivePingsDuringSilence verifies ping events are emitted
// while the upstream is quiet, keeping idle-timeout middleboxes away.
func TestStreamWriterKeepalivePingsDuringSilence(t *testing.T) {
	upstream := &slowReader{delay: 80 * time.Millisecond}
	var output strings.Builder
	writer := NewStreamWriter(&output, nil, "m", false)
	writer.keepalive = 20 * time.Millisecond
	err := writer.Consume(upstream)
	if err == nil {
		t.Fatal("a stream ending without [DONE] must be reported as truncated")
	}
	if got := strings.Count(output.String(), "event: ping"); got < 2 {
		t.Fatalf("pings = %d, want >= 2:\n%s", got, output.String())
	}
}

// slowReader emits one SSE chunk after a delay, then ends the stream.
type slowReader struct {
	delay time.Duration
	reads int
}

func (r *slowReader) Read(p []byte) (int, error) {
	r.reads++
	time.Sleep(r.delay)
	if r.reads == 1 {
		return copy(p, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"), nil
	}
	return 0, nilEOF()
}
