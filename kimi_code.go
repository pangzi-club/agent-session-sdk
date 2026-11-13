package agentsession

// kimi_code.go reads Kimi Code Desktop's newer store:
// ~/.kimi-code/sessions/wd_<workspace>_<hash>/session_<uuid>/agents/<agent>/wire.jsonl.
// State metadata is authoritative; session_index.jsonl supplies workspace
// fallbacks for incomplete state files.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type kimiCodeIndexEntry struct {
	workspace string
}

type kimiCodeState struct {
	ID         string  `json:"id"`
	Cwd        string  `json:"cwd"`
	Title      string  `json:"title"`
	LastPrompt string  `json:"lastPrompt"`
	CreatedAt  float64 `json:"createdAt"`
	UpdatedAt  float64 `json:"updatedAt"`
}

// ScanKimiCode scans the desktop app's per-agent wire transcripts.
func ScanKimiCode(agentDir string) ([]SessionInfo, error) {
	index := loadKimiCodeIndex(filepath.Join(agentDir, "session_index.jsonl"))
	projectDirs := ListDirs(filepath.Join(agentDir, "sessions"))
	results, err := MapLimit(projectDirs, FileConcurrency, func(projectDir string) ([]SessionInfo, error) {
		sessionDirs := ListDirs(projectDir)
		sessionResults, err := MapLimit(sessionDirs, FileConcurrency, func(sessionDir string) ([]SessionInfo, error) {
			state := loadKimiCodeState(filepath.Join(sessionDir, "state.json"))
			agentDirs := ListDirs(filepath.Join(sessionDir, "agents"))
			items, err := MapLimit(agentDirs, FileConcurrency, func(agentDir string) (*SessionInfo, error) {
				return scanKimiCodeAgent(sessionDir, agentDir, state, index)
			})
			if err != nil {
				return nil, err
			}
			out := []SessionInfo{}
			for _, item := range items {
				if item != nil {
					out = append(out, *item)
				}
			}
			return out, nil
		})
		if err != nil {
			return nil, err
		}
		out := []SessionInfo{}
		for _, sessions := range sessionResults {
			out = append(out, sessions...)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	out := []SessionInfo{}
	for _, sessions := range results {
		out = append(out, sessions...)
	}
	return out, nil
}

// loadKimiCodeIndex maps session id to its registered working directory.
func loadKimiCodeIndex(file string) map[string]kimiCodeIndexEntry {
	out := map[string]kimiCodeIndexEntry{}
	_ = scanJSONLines(file, func(obj map[string]any) {
		id := firstString(obj, "sessionId")
		workspace := firstString(obj, "workDir")
		if id != "" && workspace != "" {
			out[id] = kimiCodeIndexEntry{workspace: workspace}
		}
	})
	return out
}

func loadKimiCodeState(file string) kimiCodeState {
	var state kimiCodeState
	if raw, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(raw, &state)
	}
	return state
}

func scanKimiCodeAgent(sessionDir, agentDir string, state kimiCodeState, index map[string]kimiCodeIndexEntry) (*SessionInfo, error) {
	wireFile := filepath.Join(agentDir, "wire.jsonl")
	stat, err := os.Stat(wireFile)
	if err != nil || stat.IsDir() {
		return nil, nil
	}

	workspace := state.Cwd
	if workspace == "" && state.ID != "" {
		if entry, ok := index[state.ID]; ok {
			workspace = entry.workspace
		}
	}

	lines, err := ReadHeadLines(wireFile, 64*1024)
	if err != nil {
		return nil, err
	}
	title := ToTitle(state.Title)
	if title == nil {
		title = ToTitle(state.LastPrompt)
	}
	if title == nil {
		title = ParseLines(lines, func(obj map[string]any) *string {
			if !IsStr(obj["type"], "agent.message.appended") {
				return nil
			}
			message := GetMap(GetMap(obj["message"])["message"])
			if !IsStr(message["role"], "user") {
				return nil
			}
			return ToTitle(ContentToText(message["content"]))
		})
	}

	id := filepath.Base(sessionDir)
	if agentName := filepath.Base(agentDir); agentName != "" && agentName != "main" {
		id += ":" + agentName
	}
	mtime := FloorMtimeMs(stat)
	createdAt, updatedAt := mtime, mtime
	if state.CreatedAt > 0 {
		createdAt = state.CreatedAt
	}
	if state.UpdatedAt > 0 {
		updatedAt = state.UpdatedAt
	}
	return &SessionInfo{
		ID:        id,
		Workspace: workspace,
		Title:     title,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		SizeBytes: float64(stat.Size()),
		FilePath:  &wireFile,
	}, nil
}

// parseKimiCodeTranscript reads the desktop protocol's agent.message.appended
// records, matches role tool messages back onto their calls by toolCallId,
// and keeps think parts plus bounded raw tool payloads.
func parseKimiCodeTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	var out []Entry
	results := map[string]kimiCodeResult{}
	err = scanJSONLines(file, func(obj map[string]any) {
		ts := epochMs(obj["time"])
		switch firstString(obj, "type") {
		case "agent.message.appended":
			outer := GetMap(obj["message"])
			message := GetMap(outer["message"])
			if strings.EqualFold(firstString(message, "role"), "tool") {
				kimiCodeApplyResult(out, message, results)
				return
			}
			role := roleFromString(firstString(message, "role"))
			if role == "" || ts == 0 {
				return
			}
			out = append(out, entriesFromContent(role, message["content"], ts)...)
			for _, raw := range GetSlice(message["toolCalls"]) {
				call := GetMap(raw)
				name := firstString(call, "name", "tool", "tool_name")
				if name == "" {
					continue
				}
				input := argumentsMap(call["arguments"])
				tool := toolCall(name, input)
				tool.ID = firstString(call, "id", "toolCallId", "tool_use_id")
				if input != nil {
					tool.Input = clipText(compactJSON(input), MaxToolInputChars)
				}
				out = append(out, Entry{Role: RoleAssistant, Tools: []ToolCall{tool}, Timestamp: ts})
				if tool.ID != "" {
					results[tool.ID] = kimiCodeResult{}
				}
			}
		case "turn.ended":
			if duration, ok := AsNumber(obj["durationMs"]); ok && len(out) > 0 {
				out[len(out)-1].DurationMs = duration
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type kimiCodeResult struct {
	output string
	failed bool
}

// kimiCodeApplyResult updates the most recent matching call in place; result
// messages always follow their assistant toolCalls record.
func kimiCodeApplyResult(out []Entry, message map[string]any, results map[string]kimiCodeResult) {
	id := firstString(message, "toolCallId", "tool_use_id", "id")
	if id == "" {
		return
	}
	failed := AsBoolTrue(message["isError"]) || AsBoolTrue(message["failed"])
	if status := firstString(message, "status"); status != "" {
		failed = failed || status == "error"
	}
	output := ""
	if text := ContentToText(message["content"]); text != nil {
		output = clipText(*text, MaxToolOutputChars)
	}
	results[id] = kimiCodeResult{output: output, failed: failed}
	for i := len(out) - 1; i >= 0; i-- {
		for j := range out[i].Tools {
			if out[i].Tools[j].ID == id {
				out[i].Tools[j].Output = output
				out[i].Tools[j].Failed = failed
				delete(results, id)
				return
			}
		}
	}
}

// epochMs converts a millisecond field without accidentally applying the old
// Kimi wire protocol's seconds conversion.
func epochMs(v any) float64 {
	if ms := AsEpochMs(v); ms != nil {
		return *ms
	}
	return 0
}
