package agentsession

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

// parseDshTranscript decompresses a DSH session and reads its event stream.
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
		if firstString(obj, "type") == "agent/inbox/spliced" {
			data := GetMap(obj["data"])
			for _, raw := range GetSlice(data["inserted"]) {
				item := GetMap(raw)
				role := roleFromString(firstString(item, "role"))
				if role == "" {
					continue
				}
				out = append(out, entriesFromContent(role, item["content"], ts)...)
			}
			continue
		}
		out = append(out, genericEntries(obj)...)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
