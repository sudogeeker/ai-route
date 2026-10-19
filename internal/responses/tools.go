package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type tool struct {
	Name, Namespace, Wire string
	Custom                bool
	Definition            json.RawMessage
}

type toolRegistry struct {
	tools  []*tool
	byWire map[string]*tool
	byName map[string]*tool
}

func newToolRegistry() *toolRegistry {
	return &toolRegistry{byWire: make(map[string]*tool), byName: make(map[string]*tool)}
}

func toolKey(name, namespace string) string { return namespace + "\x00" + name }

// wireName keeps the alias recognizable while the hash handles long names,
// namespaces and reserved collisions. Chat tool names are limited to 64
// characters, so the readable part is capped.
func wireName(name, namespace string) string {
	base := name
	if namespace != "" {
		base = namespace + "_" + name
	}
	base = sanitizeToolName(base)
	if len(base) > 40 {
		base = base[:40]
	}
	if base == "" {
		base = "tool"
	}
	digest := sha256.Sum256([]byte(toolKey(name, namespace)))
	return "route_" + base + "_" + hex.EncodeToString(digest[:8])
}

func sanitizeToolName(value string) string {
	var result strings.Builder
	underscore := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if valid {
			result.WriteRune(r)
			underscore = false
			continue
		}
		if !underscore {
			result.WriteByte('_')
			underscore = true
		}
	}
	return strings.Trim(result.String(), "_-")
}

func (r *toolRegistry) addList(raw json.RawMessage, namespace string) error {
	if !present(raw) {
		return nil
	}
	var encoded []json.RawMessage
	if json.Unmarshal(raw, &encoded) != nil {
		var single map[string]json.RawMessage
		if json.Unmarshal(raw, &single) != nil || single == nil {
			return nil
		}
		encoded = []json.RawMessage{raw}
	}
	for _, encodedItem := range encoded {
		var item map[string]json.RawMessage
		if json.Unmarshal(encodedItem, &item) != nil || item == nil {
			continue
		}
		kind := stringField(item["type"])
		// Accept the legacy Chat-shaped wrapper as well as the flattened form.
		if kind == "function" && !present(item["name"]) && present(item["function"]) {
			var nested map[string]json.RawMessage
			if json.Unmarshal(item["function"], &nested) == nil {
				for key, value := range nested {
					if !present(item[key]) {
						item[key] = value
					}
				}
			}
		}
		if kind == "" && present(item["name"]) {
			kind = "function"
		}
		if kind == "namespace" {
			name := stringField(item["name"])
			if name == "" || !present(item["tools"]) {
				continue
			}
			child := name
			if namespace != "" {
				child = namespace + "." + name
			}
			if err := r.addList(item["tools"], child); err != nil {
				return err
			}
			continue
		}
		if kind != "function" && kind != "custom" {
			// Hosted and future tool declarations have no local executor. They
			// are omitted instead of failing the whole request.
			continue
		}
		name := stringField(item["name"])
		if name == "" {
			continue
		}
		key := toolKey(name, namespace)
		if r.byName[key] != nil {
			// Prefer the first declaration. Duplicate aliases are harmless.
			continue
		}
		description := stringField(item["description"])
		t := &tool{Name: name, Namespace: namespace, Wire: wireName(name, namespace), Custom: kind == "custom"}
		function := object{"name": t.Wire, "description": name + "\n" + description}
		if namespace != "" {
			function["description"] = namespace + "." + name + "\n" + description
		}
		inputSchema := object{"type": "object", "properties": object{"input": object{"type": "string", "description": "The complete raw tool input."}}, "required": []string{"input"}, "additionalProperties": false}
		if t.Custom {
			var format struct {
				Type, Syntax, Definition string
			}
			_ = json.Unmarshal(item["format"], &format)
			switch {
			case format.Type == "grammar" && format.Syntax == "regex":
				pattern := "^(?:" + format.Definition + ")$"
				if _, err := regexp.Compile(pattern); err != nil {
					return fmt.Errorf("custom tool %q regex is invalid: %w", name, err)
				}
				function["parameters"] = object{"type": "object", "properties": object{"input": object{"type": "string", "pattern": pattern}}, "required": []string{"input"}, "additionalProperties": false}
			default:
				// Lark grammars, missing formats and future grammar dialects
				// have no Chat equivalent. Keep the tool callable as raw text.
				function["parameters"] = inputSchema
			}
		} else {
			parameters := item["parameters"]
			var schema map[string]json.RawMessage
			if !present(parameters) || json.Unmarshal(parameters, &schema) != nil || schema == nil {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			function["parameters"] = parameters
			if present(item["strict"]) {
				function["strict"] = item["strict"]
			}
		}
		t.Definition = marshal(object{"type": "function", "function": function})
		r.byName[key], r.byWire[t.Wire] = t, t
		r.tools = append(r.tools, t)
	}
	return nil
}

// definitionsFor returns the caller tool declarations that may be used for the
// request. Chat Completions has no allowed_tools equivalent, so narrowing the
// upstream tool list is the only way to enforce a tool_choice subset before a
// tool call is emitted.
func (r *toolRegistry) definitionsFor(allowed map[string]bool) []json.RawMessage {
	definitions := make([]json.RawMessage, 0, len(r.tools))
	for _, t := range r.tools {
		if allowed[t.Wire] {
			definitions = append(definitions, t.Definition)
		}
	}
	return definitions
}

func (r *toolRegistry) resolve(ref map[string]json.RawMessage) (*tool, error) {
	var name, namespace string
	json.Unmarshal(ref["name"], &name)
	json.Unmarshal(ref["namespace"], &namespace)
	if t := r.byName[toolKey(name, namespace)]; t != nil {
		return t, nil
	}
	return nil, fmt.Errorf("Tool %q in namespace %q is not declared", name, namespace)
}

func (r *toolRegistry) choice(raw json.RawMessage) (any, map[string]bool, error) {
	allowed := make(map[string]bool)
	mode := "auto"
	if present(raw) {
		if err := json.Unmarshal(raw, &mode); err == nil {
			switch mode {
			case "none":
				return "none", allowed, nil
			case "auto", "required":
			default:
				if t := r.find(mode, ""); t != nil {
					allowed[t.Wire] = true
					return namedToolChoice(t), allowed, nil
				}
				mode = "auto"
			}
		} else {
			var ref map[string]json.RawMessage
			if json.Unmarshal(raw, &ref) != nil {
				return "auto", nil, nil
			}
			switch stringField(ref["type"]) {
			case "function", "custom":
				if t, err := r.resolve(ref); err == nil {
					allowed[t.Wire] = true
					return namedToolChoice(t), allowed, nil
				}
				mode = "auto"
			case "allowed_tools":
				if err := json.Unmarshal(ref["mode"], &mode); err != nil || (mode != "auto" && mode != "required") {
					mode = "auto"
				}
				var refs []json.RawMessage
				if json.Unmarshal(ref["tools"], &refs) == nil {
					for _, item := range refs {
						var entry map[string]json.RawMessage
						if json.Unmarshal(item, &entry) == nil {
							if t, err := r.resolve(entry); err == nil {
								allowed[t.Wire] = true
							}
						} else {
							var name string
							if json.Unmarshal(item, &name) == nil {
								if t := r.find(name, ""); t != nil {
									allowed[t.Wire] = true
								}
							}
						}
					}
				}
				if len(allowed) == 0 {
					return "none", allowed, nil
				}
				return mode, allowed, nil
			default:
				if t, err := r.resolve(ref); err == nil {
					allowed[t.Wire] = true
					return namedToolChoice(t), allowed, nil
				}
				mode = "auto"
			}
		}
	}
	if mode != "auto" && mode != "required" {
		mode = "auto"
	}
	for name := range r.byWire {
		allowed[name] = true
	}
	if len(allowed) == 0 {
		return "none", allowed, nil
	}
	return mode, allowed, nil
}

func namedToolChoice(t *tool) object {
	return object{"type": "function", "function": object{"name": t.Wire}}
}

func (r *toolRegistry) find(name, namespace string) *tool {
	if t := r.byName[toolKey(name, namespace)]; t != nil {
		return t
	}
	if t := r.byWire[name]; t != nil {
		return t
	}
	for _, t := range r.tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}
