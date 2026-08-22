package translate

import (
	"io"
	"strings"
	"testing"
	"time"
)

func nilEOF() error { return io.EOF }

// TestStreamWriterEmptyUpstreamStillCompletes covers a 200 from upstream that
// carries no events at all: the client must still receive a complete message
// envelope instead of a silent stream it waits on forever.
func TestStreamWriterEmptyUpstreamStillCompletes(t *testing.T) {
	var output strings.Builder
	writer := NewStreamWriter(&output, nil, "m", false)
	if err := writer.Consume(strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	body := output.String()
	for _, want := range []string{"event: message_start", "event: content_block_start", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("missing stop_reason:\n%s", body)
	}
}

// TestStreamWriterKeepalivePingsDuringSilence verifies ping events are emitted
// while the upstream is quiet, keeping idle-timeout middleboxes away.
func TestStreamWriterKeepalivePingsDuringSilence(t *testing.T) {
	upstream := &slowReader{delay: 80 * time.Millisecond}
	var output strings.Builder
	writer := NewStreamWriter(&output, nil, "m", false)
	writer.keepalive = 20 * time.Millisecond
	if err := writer.Consume(upstream); err != nil {
		t.Fatal(err)
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
