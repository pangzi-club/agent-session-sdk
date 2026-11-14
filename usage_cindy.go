package agentsession

import "fmt"

// Cindy sessions can use Pi, Codex or Claude Code. Each engine owns its token
// format, while records retain Cindy's profile ID and agent ID.

func scanCindyUsage(agentDir string) (*usageModuleResult, error) {
	sessions, err := ScanCindy(agentDir)
	if err != nil {
		return nil, err
	}
	results, err := MapLimit(sessions, FileConcurrency, scanCindySessionUsage)
	if err != nil {
		return nil, err
	}
	outSessions := []SessionInfo{}
	records := []UsageRecord{}
	for _, result := range results {
		outSessions = append(outSessions, result.session)
		records = append(records, result.records...)
	}
	return &usageModuleResult{sessions: outSessions, records: records}, nil
}

type cindySessionUsage struct {
	session SessionInfo
	records []UsageRecord
}

func scanCindySessionUsage(session SessionInfo) (*cindySessionUsage, error) {
	result := &cindySessionUsage{session: session, records: []UsageRecord{}}
	if session.FilePath == nil || *session.FilePath == "" {
		return result, nil
	}
	switch session.transcriptFormat {
	case "pi":
		usage, err := scanPiSessionUsage(session, "cindy")
		if err != nil {
			return nil, err
		}
		result.records = usage.records
	case "codex":
		usage, err := scanCodexSessionUsage(session, "cindy")
		if err != nil {
			return nil, err
		}
		result.records = usage
	case "claude":
		usage, err := scanClaudeSessionUsage(session, "cindy")
		if err != nil {
			return nil, err
		}
		result.records = usage
	default:
		return nil, fmt.Errorf("session %q has unsupported Cindy usage format %q", session.ID, session.transcriptFormat)
	}
	return result, nil
}
