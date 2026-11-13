package agentsession

// usage_cindy.go reuses Pi's usage extraction because Cindy writes its visible
// turns and provider usage to the same JSONL event format. Session IDs remain
// Cindy's profile IDs so consumer-side aggregates do not collide with Pi.

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
	result, err := scanPiSessionUsage(session, "cindy")
	if err != nil {
		return nil, err
	}
	return &cindySessionUsage{session: session, records: result.records}, nil
}
