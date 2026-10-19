package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTruncateMessagesAutoKeepsToolPair(t *testing.T) {
	messages := []object{
		{"role": "system", "content": "system"},
		{"role": "user", "content": "old-" + strings.Repeat("x", truncationAutoHistoryBytes+1024)},
		{"role": "assistant", "content": nil, "tool_calls": []any{object{
			"id": "call_1", "type": "function", "function": object{"name": "route_echo", "arguments": "{}"},
		}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "tool result"},
		{"role": "user", "content": "latest"},
	}
	got := truncateMessages(messages, json.RawMessage(`"auto"`))
	if len(got) != 4 {
		t.Fatalf("unexpected truncation result: %#v", got)
	}
	if got[0]["role"] != "system" || got[1]["role"] != "assistant" || got[2]["role"] != "tool" || got[3]["role"] != "user" {
		t.Fatalf("tool call and output were split or latest user was lost: %#v", got)
	}
}

func TestTruncateMessagesAutoNeverKeepsOrphanToolOutput(t *testing.T) {
	messages := []object{
		{"role": "assistant", "content": nil, "tool_calls": []any{object{
			"id": "call_1", "type": "function", "function": object{"name": "route_echo", "arguments": "{}"},
		}}},
		{"role": "tool", "tool_call_id": "call_1", "content": strings.Repeat("x", truncationAutoHistoryBytes+1024)},
		{"role": "user", "content": "latest"},
	}
	got := truncateMessages(messages, json.RawMessage(`"auto"`))
	for _, message := range got {
		if message["role"] == "tool" {
			t.Fatalf("truncation left an orphan tool output: %#v", got)
		}
	}
	if len(got) != 1 || got[0]["content"] != "latest" {
		t.Fatalf("latest user turn was not retained: %#v", got)
	}
}

func TestTruncateMessagesAutoKeepsCompactionSummary(t *testing.T) {
	messages := []object{
		{"role": "user", "content": "Conversation summary:\nkept summary"},
		{"role": "user", "content": "old-" + strings.Repeat("x", truncationAutoHistoryBytes+1024)},
		{"role": "user", "content": "latest"},
	}
	got := truncateMessages(messages, json.RawMessage(`"auto"`))
	if len(got) != 2 || !strings.HasPrefix(got[0]["content"].(string), "Conversation summary:") || got[1]["content"] != "latest" {
		t.Fatalf("compaction summary or latest turn was lost: %#v", got)
	}
}

func TestTruncateMessagesLeavesDisabledHistoryUntouched(t *testing.T) {
	messages := []object{
		{"role": "user", "content": "old-" + strings.Repeat("x", truncationAutoHistoryBytes+1024)},
		{"role": "user", "content": "latest"},
	}
	got := truncateMessages(messages, json.RawMessage(`"disabled"`))
	if len(got) != len(messages) {
		t.Fatalf("disabled truncation changed history: %#v", got)
	}
}
