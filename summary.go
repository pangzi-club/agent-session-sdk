package agentsession

import ()

// Summarize aggregates a per-agent usage overview: Sessions includes sessions
// without records, Requests counts records, and token totals accumulate in
// record order. Output is sorted by first appearance of each agent (session
// map order).
func Summarize(scan UsageScan) []AgentUsageSummary {
	var order []string
	byAgent := map[string]*AgentUsageSummary{}
	into := func(agentID string) *AgentUsageSummary {
		if summary, ok := byAgent[agentID]; ok {
			return summary
		}
		summary := &AgentUsageSummary{AgentID: agentID}
		byAgent[agentID] = summary
		order = append(order, agentID)
		return summary
	}
	for _, session := range scan.Sessions {
		into(session.AgentID).Sessions++
	}
	for _, record := range scan.Records {
		summary := into(record.AgentID)
		summary.Requests++
		summary.InputTokens += record.InputTokens
		summary.OutputTokens += record.OutputTokens
		summary.CacheReadTokens += record.CacheReadTokens
		summary.CacheWriteTokens += record.CacheWriteTokens
		summary.ActiveMs += record.ActiveMs
	}
	out := make([]AgentUsageSummary, 0, len(order))
	for _, agentID := range order {
		out = append(out, *byAgent[agentID])
	}
	return out
}
