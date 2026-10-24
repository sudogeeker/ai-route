package opencode

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
)

// agentSystemPrompt is the default prompt shipped by the OpenCode 1.19.4 CLI.
// The free inference gate rejects otherwise valid anonymous requests that omit
// it or send the 2.x prompt instead.
//
//go:embed system_prompt.txt
var agentSystemPrompt string

const agentSkills = `Skills provide specialized instructions and workflows for specific tasks.
Use the skill tool to load a skill when a task matches its description.
<available_skills>
  <skill>
    <name>customize-opencode</name>
    <description>Use ONLY when the user is editing or creating opencode's own configuration: opencode.json, opencode.jsonc, files under .opencode/, or files under ~/.config/opencode/. Also use when creating or fixing opencode agents, subagents, skills, plugins, MCP servers, or permission rules. Do not use for the user's own application code, or for any project that is not configuring opencode itself.</description>
    <location>&lt;built-in&gt;</location>
  </skill>
</available_skills>`

func agentEnvironment(model string) string {
	workingDirectory := strings.TrimSpace(os.Getenv("OPENCODE_WORKSPACE"))
	if workingDirectory == "" {
		workingDirectory, _ = os.Getwd()
	}
	workingDirectory = filepath.Clean(workingDirectory)
	workspaceRoot := strings.TrimSpace(os.Getenv("OPENCODE_WORKSPACE_ROOT"))
	if workspaceRoot == "" {
		workspaceRoot = workingDirectory
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	gitRepository := "no"
	if _, err := os.Stat(filepath.Join(workspaceRoot, ".git")); err == nil {
		gitRepository = "yes"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	apiModel := strings.TrimPrefix(model, "opencode/")
	if apiModel == "" {
		apiModel = model
	}
	return strings.Join([]string{
		"You are powered by the model named " + apiModel + ". The exact model ID is opencode/" + apiModel,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + workingDirectory,
		"  Workspace root folder: " + workspaceRoot,
		"  Is directory a git repo: " + gitRepository,
		"  Platform: " + platform,
		"  Today's date: " + time.Now().Format("Mon Jan 02 2006"),
		"</env>",
	}, "\n")
}

func agentSystemContent(model, instructions string) string {
	parts := []string{agentSystemPrompt, agentEnvironment(model)}
	if instructions = strings.TrimSpace(instructions); instructions != "" {
		parts = append(parts, instructions)
	}
	parts = append(parts, agentSkills)
	return strings.Join(parts, "\n")
}

func agentPromptMessage(model, instructions string) json.RawMessage {
	content, _ := json.Marshal(agentSystemContent(model, instructions))
	return json.RawMessage(`{"role":"system","content":` + string(content) + `}`)
}

// injectAgentPrompt builds the single system message used by the 1.19.4 CLI.
// Caller system content is kept in order after the environment block and before
// the skills block, matching the CLI's provider/system/instructions assembly.
func injectAgentPrompt(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return body, nil
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	var messages []json.RawMessage
	if raw := fields["messages"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, err
		}
	}
	kept := make([]json.RawMessage, 0, len(messages)+1)
	var instructions []string
	for _, raw := range messages {
		var message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &message) == nil && message.Role == "system" {
			if text, ok := systemContentText(message.Content); ok {
				if text = strings.TrimSpace(text); text != "" {
					instructions = append(instructions, text)
				}
				continue
			}
		}
		kept = append(kept, raw)
	}
	messages = append([]json.RawMessage{agentPromptMessage(model, strings.Join(instructions, "\n"))}, kept...)
	encoded, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	fields["messages"] = encoded
	normalizeTokenLimit(fields)
	return json.Marshal(fields)
}

func systemContentText(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if (part.Type == "" || part.Type == "text" || part.Type == "input_text") && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	if len(texts) == 0 {
		return "", true
	}
	return strings.Join(texts, "\n"), true
}

func normalizeTokenLimit(fields map[string]json.RawMessage) {
	for _, key := range []string{"max_completion_tokens", "max_output_tokens"} {
		if raw, ok := fields[key]; ok {
			if _, present := fields["max_tokens"]; !present {
				fields["max_tokens"] = raw
			}
			delete(fields, key)
		}
	}
	if _, ok := fields["max_tokens"]; !ok {
		fields["max_tokens"] = json.RawMessage(strconv.Itoa(config.DefaultOpenCodeMaxTokens))
	}
}
