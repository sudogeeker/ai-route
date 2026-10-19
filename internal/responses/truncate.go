package responses

import (
	"encoding/json"
	"strings"
)

// truncationAutoHistoryBytes bounds the stateless Chat history when the caller
// requests truncation:"auto". It approximates a 96k-token history, leaving
// room for tool declarations, instructions, and the generated answer.
const truncationAutoHistoryBytes = 384 << 10

type messageGroup struct {
	indexes  []int
	size     int
	toolTurn bool
}

// truncateMessages implements the safe part of Responses truncation:"auto".
// System instructions, compaction summaries, and the newest user turn are
// always kept. Older messages are removed by complete groups so an assistant
// tool_calls message is never separated from its tool outputs. If the history
// is already bounded, or no safe candidate exists, it is returned unchanged.
func truncateMessages(messages []object, truncation json.RawMessage) []object {
	if stringField(truncation) != "auto" || len(messages) == 0 {
		return messages
	}
	groups, total := groupMessages(messages)
	if total <= truncationAutoHistoryBytes {
		return messages
	}

	keep := make([]bool, len(groups))
	for groupIndex, group := range groups {
		for _, messageIndex := range group.indexes {
			message := messages[messageIndex]
			if stringFieldValue(message["role"]) == "system" || isCompactionSummaryMessage(message) {
				keep[groupIndex] = true
				break
			}
		}
	}
	if latest := latestUserMessage(messages); latest >= 0 {
		for groupIndex, group := range groups {
			for _, messageIndex := range group.indexes {
				if messageIndex >= latest {
					keep[groupIndex] = true
					break
				}
			}
		}
	}

	used := 0
	for groupIndex, kept := range keep {
		if kept {
			used += groups[groupIndex].size
		}
	}
	for groupIndex := len(groups) - 1; groupIndex >= 0; groupIndex-- {
		if keep[groupIndex] {
			continue
		}
		size := groups[groupIndex].size
		if used+size > truncationAutoHistoryBytes {
			continue
		}
		keep[groupIndex] = true
		used += size
	}

	trimmed := make([]object, 0, len(messages))
	for groupIndex, group := range groups {
		if !keep[groupIndex] {
			continue
		}
		for _, messageIndex := range group.indexes {
			trimmed = append(trimmed, messages[messageIndex])
		}
	}
	if len(trimmed) == 0 || len(trimmed) == len(messages) {
		return messages
	}
	return trimmed
}

func groupMessages(messages []object) ([]messageGroup, int) {
	groups := make([]messageGroup, 0, len(messages))
	total := 0
	for messageIndex, message := range messages {
		size := estimatedMessageBytes(message)
		total += size
		role := stringFieldValue(message["role"])
		if role == "tool" && len(groups) != 0 && groups[len(groups)-1].toolTurn {
			group := &groups[len(groups)-1]
			group.indexes = append(group.indexes, messageIndex)
			group.size += size
			continue
		}
		groups = append(groups, messageGroup{
			indexes:  []int{messageIndex},
			size:     size,
			toolTurn: role == "assistant" && hasToolCalls(message),
		})
	}
	return groups, total
}

func estimatedMessageBytes(message object) int {
	data, err := json.Marshal(message)
	if err != nil {
		return 0
	}
	return len(data)
}

func hasToolCalls(message object) bool {
	calls, ok := message["tool_calls"].([]any)
	return ok && len(calls) != 0
}

func latestUserMessage(messages []object) int {
	fallback := -1
	for messageIndex := len(messages) - 1; messageIndex >= 0; messageIndex-- {
		message := messages[messageIndex]
		if stringFieldValue(message["role"]) != "user" {
			continue
		}
		if fallback < 0 {
			fallback = messageIndex
		}
		if !attachmentOnlyMessage(message) {
			return messageIndex
		}
	}
	return fallback
}

func attachmentOnlyMessage(message object) bool {
	content, ok := message["content"].([]any)
	if !ok {
		return false
	}
	if len(content) == 0 {
		return true
	}
	for _, rawPart := range content {
		part, ok := rawPart.(object)
		if !ok {
			return false
		}
		if stringFieldValue(part["type"]) == "text" && strings.TrimSpace(stringFieldValue(part["text"])) != "" {
			return false
		}
	}
	return true
}

func isCompactionSummaryMessage(message object) bool {
	if stringFieldValue(message["role"]) != "user" {
		return false
	}
	switch content := message["content"].(type) {
	case string:
		return strings.HasPrefix(content, "Conversation summary:\n")
	case []any:
		for _, rawPart := range content {
			part, ok := rawPart.(object)
			if !ok {
				continue
			}
			if strings.HasPrefix(stringFieldValue(part["text"]), "Conversation summary:\n") {
				return true
			}
		}
	}
	return false
}
