package responses

import (
	"strings"
	"testing"
)

func TestChatStreamLimits(t *testing.T) {
	line := ":" + strings.Repeat("x", 4094) + "\n"
	stream := newChatStream(strings.NewReader(strings.Repeat(line, maxSSELine/len(line)) + "\n"))
	if _, err := stream.next(); err == nil || err.Error() != "Chat stream event exceeds buffer limit" {
		t.Fatalf("oversized multiline event: %v", err)
	}
	stream = newChatStream(strings.NewReader("data: {}\n\ndata: {}\n\n"))
	stream.total = maxResponseStream - len("data: {}\n\n")
	if _, err := stream.next(); err != nil {
		t.Fatalf("event at the response limit: %v", err)
	}
	if _, err := stream.next(); err == nil || err.Error() != "Chat stream response exceeds buffer limit" {
		t.Fatalf("response limit not enforced across events: %v", err)
	}
}
