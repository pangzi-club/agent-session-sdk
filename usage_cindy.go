package agentsession

import (
	"errors"
	"fmt"
	"os"
)

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

// Cindy session transcripts are stored as absolute paths in the profile DB and
// can drift out of sync with disk (archived or deleted while the row stays
// active). A missing or unreadable transcript skips that one session instead
// of failing the whole module: a stale row must not wipe Cindy's usage for
// every other session, and the aggregation layer must not mistake that for
// "Cindy not installed".
func scanCindySessionUsage(session SessionInfo) (*cindySessionUsage, error) {
	result := &cindySessionUsage{session: session, records: []UsageRecord{}}
	if session.FilePath == nil || *session.FilePath == "" {
		return result, nil
	}
	if info, err := os.Stat(*session.FilePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
		return result, nil
	} else if !info.Mode().IsRegular() {
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
