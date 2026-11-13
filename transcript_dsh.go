package agentsession

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

// parseDshTranscript decompresses a DSH session and reads its event stream.
// User text arrives twice (an agent/inbox/spliced insert, then the user/message
// echo) and tool calls twice (an assistant content block, then the tool/call
// event); both are deduplicated by message / call id. tool/result events fill
// the emitted cards' Output, Failed and DurationMs.
func parseDshTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	decoded, err := zstdDecoder.DecodeAll(raw, nil)
	if err != nil {
		return nil, err
	}

	var out []Entry
	seenUserIDs := map[string]bool{}
	emittedCalls := map[string]bool{}
	callTimes := map[string]float64{}
	markCalls := func(entries []Entry) {
		for _, entry := range entries {
			for _, tool := range entry.Tools {
				if tool.ID != "" {
					emittedCalls[tool.ID] = true
				}
			}
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(decoded))
	scanner.Buffer(make([]byte, 64*1024), ScanMaxTranscriptLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		ts := mapTimestamp(obj, false)
		data := GetMap(obj["data"])
		switch firstString(obj, "type") {
		case "agent/inbox/spliced":
			for _, raw := range GetSlice(data["inserted"]) {
				item := GetMap(raw)
				role := roleFromString(firstString(item, "role"))
				if role == "" {
					continue
				}
				if role == RoleUser && (dshInjectedUser(item) || !firstUserID(seenUserIDs, item)) {
					continue
				}
				entries := entriesFromContent(role, item["content"], ts)
				markCalls(entries)
				out = append(out, entries...)
			}
		case "user/message":
			if dshInjectedUser(data) || !firstUserID(seenUserIDs, data) {
				continue
			}
			role := roleFromString(firstString(data, "role"))
			if role == "" {
				role = RoleUser
			}
			out = append(out, entriesFromContent(role, data["content"], ts)...)
		case "assistant/message":
			message := GetMap(data["message"])
			entries := entriesFromContent(RoleAssistant, message["content"], ts)
			markCalls(entries)
			out = append(out, entries...)
		case "tool/call":
			id := firstString(data, "callId", "id")
			if id != "" {
				callTimes[id] = ts
				if emittedCalls[id] {
					continue
				}
			}
			// The assistant content blocks normally carry the call already;
			// this covers stores where the tool/call event is the only source.
			call, ok := toolFromBlock(map[string]any{"name": data["name"], "arguments": data["arguments"], "id": id})
			if !ok {
				continue
			}
			if id != "" {
				emittedCalls[id] = true
			}
			out = append(out, Entry{Role: RoleAssistant, Tools: []ToolCall{call}, Timestamp: ts})
		case "tool/result":
			applyDshToolResults(out, data, ts, callTimes)
		default:
			out = append(out, genericEntries(obj)...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// dshInjectedSources are harness-injected context kinds that arrive as user
// turns but are not conversation: workspace instructions, skill catalogs and
// runtime snapshots. Real input (user), background-job and approval notices
// (tool-jobs / user-approval) and model-change notices stay visible.
var dshInjectedSources = map[string]bool{
	"agent-instructions": true,
	"skill-catalog":      true,
	"skill-invocation":   true,
	"runtime-context":    true,
	"plugin":             true,
}

func dshInjectedUser(obj map[string]any) bool {
	return dshInjectedSources[firstString(GetMap(obj["source"]), "kind")]
}

// firstUserID reports whether the object's id is new, recording it. Empty ids
// always pass: without an id there is nothing to deduplicate on.
func firstUserID(seen map[string]bool, obj map[string]any) bool {
	id := firstString(obj, "id")
	if id == "" {
		return true
	}
	if seen[id] {
		return false
	}
	seen[id] = true
	return true
}

// applyDshToolResults pairs a tool/result event onto emitted tool cards. v3
// wraps each result in a tool-result content block with its own call id; v4
// puts flat text blocks and the call id on the message itself.
func applyDshToolResults(out []Entry, data map[string]any, ts float64, callTimes map[string]float64) {
	message := GetMap(data["message"])
	if message == nil {
		return
	}
	pair := func(id, output string, failed bool) {
		if id == "" {
			return
		}
		for i := range out {
			for j := range out[i].Tools {
				tool := &out[i].Tools[j]
				if tool.ID != id || tool.Output != "" {
					continue
				}
				tool.Output = clipText(output, MaxToolOutputChars)
				tool.Failed = failed
				start := out[i].Timestamp
				if t, ok := callTimes[id]; ok {
					start = t
				}
				if ts > start {
					out[i].DurationMs = ts - start
				}
			}
		}
	}
	sawBlock := false
	for _, raw := range GetSlice(message["content"]) {
		block := GetMap(raw)
		if block == nil || !IsStr(block["type"], "tool-result") {
			continue
		}
		sawBlock = true
		pair(firstString(block, "toolCallId", "tool_use_id", "id"), toolResultText(block), AsBoolTrue(block["isError"]))
	}
	if sawBlock {
		return
	}
	id := firstString(message, "toolCallId")
	if id == "" {
		id = firstString(GetMap(message["source"]), "callId")
	}
	pair(id, toolResultText(message), AsBoolTrue(message["isError"]))
}
