package agentsession

// parseGenericTranscript extracts common message shapes from agent stores that
// have no dedicated parser yet.
func parseGenericTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = scanJSONLines(file, func(obj map[string]any) {
		out = append(out, genericEntries(obj)...)
	})
	return out, err
}
