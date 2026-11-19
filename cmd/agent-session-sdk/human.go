package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	agentsession "github.com/pangzi-club/agent-session-sdk"
)

func writeHuman(w io.Writer, command string, payload any) error {
	switch command {
	case "sessions", "search":
		results, ok := payload.([]agentsession.SessionScanResult)
		if !ok {
			return fmt.Errorf("unexpected %s output type %T", command, payload)
		}
		return writeHumanSessions(w, results)
	case "summary":
		summaries, ok := payload.([]agentsession.AgentUsageSummary)
		if !ok {
			return fmt.Errorf("unexpected summary output type %T", payload)
		}
		return writeHumanSummary(w, summaries)
	case "usage":
		return writeHumanUsage(w, payload)
	case "transcript":
		entries, ok := payload.([]agentsession.Entry)
		if !ok {
			return fmt.Errorf("unexpected transcript output type %T", payload)
		}
		return writeHumanTranscript(w, entries)
	default:
		return fmt.Errorf("human output is not supported for %s", command)
	}
}

func writeHumanSessions(w io.Writer, results []agentsession.SessionScanResult) error {
	count := 0
	for _, result := range results {
		if len(result.Sessions) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s (%d sessions)\n", result.AgentName, len(result.Sessions))
		for _, session := range result.Sessions {
			title := valueOr(session.Title, "(untitled)")
			fmt.Fprintf(w, "  %s  [%s]\n", title, session.ID)
			fmt.Fprintf(w, "    Updated: %s\n    Workspace: %s\n", formatEpochMs(session.UpdatedAt), emptyDash(session.Workspace))
			count++
		}
	}
	if count == 0 {
		_, err := fmt.Fprintln(w, "No sessions found.")
		return err
	}
	return nil
}

func writeHumanSummary(w io.Writer, summaries []agentsession.AgentUsageSummary) error {
	if len(summaries) == 0 {
		_, err := fmt.Fprintln(w, "No usage data found.")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSESSIONS\tREQUESTS\tINPUT TOKENS\tOUTPUT TOKENS\tCACHE READ\tCACHE WRITE\tACTIVE TIME")
	for _, s := range summaries {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			s.AgentID, s.Sessions, s.Requests, formatCount(s.InputTokens), formatCount(s.OutputTokens),
			formatCount(s.CacheReadTokens), formatCount(s.CacheWriteTokens), formatDuration(s.ActiveMs))
	}
	return tw.Flush()
}

func writeHumanUsage(w io.Writer, payload any) error {
	type usageRow struct {
		record agentsession.UsageRecord
		cost   *float64
	}
	var records []usageRow
	var sessions []agentsession.UsageSession
	switch scan := payload.(type) {
	case agentsession.UsageScan:
		sessions = scan.Sessions
		for _, record := range scan.Records {
			records = append(records, usageRow{record: record})
		}
	case costScan:
		sessions = scan.Sessions
		for _, record := range scan.Records {
			records = append(records, usageRow{record: record.UsageRecord, cost: record.CostUsd})
		}
	default:
		return fmt.Errorf("unexpected usage output type %T", payload)
	}
	if len(records) == 0 {
		_, err := fmt.Fprintln(w, "No usage records found.")
		return err
	}
	titles := make(map[string]string, len(sessions))
	for _, session := range sessions {
		titles[usageSessionKey(session.AgentID, session.ID)] = valueOr(session.Title, "(untitled)")
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tAGENT\tMODEL\tSESSION\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tCOST")
	for _, row := range records {
		record := row.record
		model := valueOr(record.Model, "-")
		title := titles[usageSessionKey(record.AgentID, record.SessionID)]
		if title == "" {
			title = record.SessionID
		}
		costText := "-"
		if row.cost != nil {
			costText = "$" + strconv.FormatFloat(*row.cost, 'f', 4, 64)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			formatEpochMs(record.Timestamp), record.AgentID, model, title,
			formatCount(record.InputTokens), formatCount(record.OutputTokens),
			formatCount(record.CacheReadTokens), formatCount(record.CacheWriteTokens), costText)
	}
	return tw.Flush()
}

func writeHumanTranscript(w io.Writer, entries []agentsession.Entry) error {
	if len(entries) == 0 {
		_, err := fmt.Fprintln(w, "Transcript is empty.")
		return err
	}
	for i, entry := range entries {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		role := strings.ToUpper(string(entry.Role))
		if role == "" {
			role = "MESSAGE"
		}
		if entry.Timestamp > 0 {
			fmt.Fprintf(w, "%s  %s\n", role, formatEpochMs(entry.Timestamp))
		} else {
			fmt.Fprintf(w, "%s\n", role)
		}
		if entry.Text != "" {
			fmt.Fprintf(w, "%s\n", indent(entry.Text, "  "))
		}
		if entry.Thinking != "" {
			fmt.Fprintf(w, "  Thinking:\n%s\n", indent(entry.Thinking, "    "))
		}
		for _, tool := range entry.Tools {
			label := tool.Name
			if tool.Failed {
				label += " (failed)"
			}
			fmt.Fprintf(w, "  Tool: %s\n", label)
			if tool.Title != "" {
				fmt.Fprintf(w, "    %s\n", tool.Title)
			}
			if tool.Input != "" {
				fmt.Fprintf(w, "    Input: %s\n", indent(tool.Input, "      "))
			}
			if tool.Output != "" {
				fmt.Fprintf(w, "    Output: %s\n", indent(tool.Output, "      "))
			}
		}
	}
	return nil
}

func usageSessionKey(agentID, sessionID string) string {
	return agentID + "\x00" + sessionID
}

func valueOr(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func formatCount(value float64) string {
	return strconv.FormatFloat(value, 'f', 0, 64)
}

func formatEpochMs(value float64) string {
	if value <= 0 {
		return "-"
	}
	return time.UnixMilli(int64(value)).Local().Format("2006-01-02 15:04")
}

func formatDuration(milliseconds float64) string {
	if milliseconds <= 0 {
		return "-"
	}
	return (time.Duration(milliseconds) * time.Millisecond).Round(time.Minute).String()
}

func indent(text, prefix string) string {
	return prefix + strings.ReplaceAll(text, "\n", "\n"+prefix)
}
