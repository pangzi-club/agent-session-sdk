// agent-session-sdk: stateless CLI over the agentsession library. Scans local
// coding-agent stores and prints session lists, usage, summaries or one
// session's transcript as JSON on stdout. Pure Go dependencies only (no cgo),
// single-file distribution, one cross-compile command away. The session and
// usage shapes stay field-compatible with agent-view's ipc-contract.ts.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pangzi-club/agent-session-sdk"
)

const helpText = `agent-session-sdk — session / usage / transcript scanner for local agent stores

Usage:
  agent-session-sdk <sessions|usage|summary|transcript> [flags]

Subcommands:
  sessions    Session list for all agents (SessionScanResult[] JSON)
  usage       Token usage records for all agents ({records, sessions} JSON)
  summary     Per-agent usage overview (sessions / requests / total tokens / active time)
  transcript  One session's transcript (Entry[] JSON, needs --agent and --session)
  help        Show this help

Flags:
  --home <dir>     Target user home directory (default $HOME)
  --agent <ids>    Only process the given agents, comma-separated (e.g. --agent codex,claude)
  --session <id>   Transcript subcommand only: the session id to load
  --cost           Usage subcommand only: append costUsd / costUnpricedTokens per record
  --pretty         Indent output for readability

Session scanning covers 14 agents: claude codex gemini cursor opencode zcode
workbuddy pi dsh grok qoder qoder-cn kimi hermes. Usage / summary cover the
agents with local usage data: claude codex zcode workbuddy dsh pi opencode grok
qoder qoder-cn hermes. Qoder / qoder-cn tokens are heuristically reconstructed
from context ratio + reply length, and records keep credits for cost
estimation — free requests included.

Examples:
  agent-session-sdk sessions --agent codex --pretty
  agent-session-sdk usage --home /Users/demo --cost | jq '.records | length'
  agent-session-sdk summary --agent codex,claude
  agent-session-sdk transcript --agent claude --session <sessionId> --pretty
`

type cliOptions struct {
	home    string
	agents  map[string]bool
	pretty  bool
	session string
	cost    bool
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(2)
	}
	command := args[0]
	if command == "-h" || command == "--help" || command == "help" {
		fmt.Print(helpText)
		return
	}

	opts := parseFlags(command, args[1:])
	var payload any
	switch command {
	case "sessions":
		payload = filterSessions(agentsession.ScanSessions(opts.home), opts.agents)
	case "usage":
		payload = costify(filterUsage(agentsession.ScanAllUsage(opts.home), opts.agents), opts.cost)
	case "summary":
		payload = filterSummaries(agentsession.Summarize(agentsession.ScanAllUsage(opts.home)), opts.agents)
	case "transcript":
		payload = loadTranscript(opts)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", command)
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(2)
	}
	if payload == nil {
		os.Exit(1)
	}

	if err := encode(os.Stdout, payload, opts.pretty); err != nil {
		fmt.Fprintf(os.Stderr, "failed to serialize output: %v\n", err)
		os.Exit(1)
	}
}

// parseFlags handles the common flags (--home / --agent / --pretty) plus the
// per-subcommand ones (--session, --cost), and validates agent ids.
func parseFlags(command string, args []string) *cliOptions {
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, helpText) }
	opts := &cliOptions{agents: map[string]bool{}}
	fs.StringVar(&opts.home, "home", "", "target user home directory (default $HOME)")
	agentList := fs.String("agent", "", "only process the given agents, comma-separated")
	fs.BoolVar(&opts.pretty, "pretty", false, "indent output")
	fs.StringVar(&opts.session, "session", "", "session id (transcript only)")
	fs.BoolVar(&opts.cost, "cost", false, "append cost fields (usage only)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unknown argument: %s\n", fs.Arg(0))
		os.Exit(2)
	}
	for _, id := range strings.Split(*agentList, ",") {
		if id = strings.TrimSpace(id); id != "" {
			if !agentsession.IsKnownAgent(id) {
				fmt.Fprintf(os.Stderr, "unknown agent: %s (valid: %s)\n", id,
					strings.Join(agentsession.KnownAgentIDs(), " "))
				os.Exit(2)
			}
			opts.agents[id] = true
		}
	}
	if command == "transcript" {
		if len(opts.agents) != 1 {
			fmt.Fprintln(os.Stderr, "transcript needs exactly one --agent")
			os.Exit(2)
		}
		if opts.session == "" {
			fmt.Fprintln(os.Stderr, "transcript needs --session <id>")
			os.Exit(2)
		}
	}
	if opts.home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot determine home directory: %v\n", err)
			os.Exit(1)
		}
		opts.home = home
	}
	return opts
}

// costRecord wraps UsageRecord with the optional cost fields. They sit outside
// the base contract: only the --cost path emits them.
type costRecord struct {
	agentsession.UsageRecord
	CostUsd            *float64 `json:"costUsd,omitempty"`
	CostUnpricedTokens *float64 `json:"costUnpricedTokens,omitempty"`
}

// costScan is the --cost shape of the usage output: records carry the cost
// fields on top of the base contract. Without --cost the plain UsageScan is
// emitted, byte-identical to the previous scanner.
type costScan struct {
	Records  []costRecord                `json:"records"`
	Sessions []agentsession.UsageSession `json:"sessions"`
}

func costify(scan agentsession.UsageScan, withCost bool) any {
	if !withCost {
		return scan
	}
	out := costScan{Records: make([]costRecord, 0, len(scan.Records)), Sessions: scan.Sessions}
	for _, record := range scan.Records {
		usd, unpriced := agentsession.EstimateTokenCost(agentsession.TokenCostInput{
			AgentID: record.AgentID, Model: record.Model,
			InputTokens: record.InputTokens, OutputTokens: record.OutputTokens,
			CacheReadTokens: record.CacheReadTokens, CacheWriteTokens: record.CacheWriteTokens,
			Credits: record.Credits,
		})
		out.Records = append(out.Records, costRecord{UsageRecord: record, CostUsd: &usd, CostUnpricedTokens: &unpriced})
	}
	return out
}

func loadTranscript(opts *cliOptions) any {
	var agentID string
	for id := range opts.agents {
		agentID = id
	}
	var info *agentsession.SessionInfo
	for _, result := range agentsession.ScanSessions(opts.home) {
		if result.AgentID != agentID {
			continue
		}
		for i := range result.Sessions {
			if result.Sessions[i].ID == opts.session {
				info = &result.Sessions[i]
				break
			}
		}
	}
	if info == nil {
		fmt.Fprintf(os.Stderr, "session %q not found for agent %q\n", opts.session, agentID)
		return nil
	}
	entries, err := agentsession.LoadTranscript(agentID, *info)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load transcript: %v\n", err)
		return nil
	}
	if entries == nil {
		return []agentsession.Entry{}
	}
	return entries
}

func filterSessions(results []agentsession.SessionScanResult, agents map[string]bool) []agentsession.SessionScanResult {
	if len(agents) == 0 {
		return results
	}
	out := make([]agentsession.SessionScanResult, 0, len(results))
	for _, result := range results {
		if agents[result.AgentID] {
			out = append(out, result)
		}
	}
	return out
}

func filterUsage(scan agentsession.UsageScan, agents map[string]bool) agentsession.UsageScan {
	if len(agents) == 0 {
		return scan
	}
	records := make([]agentsession.UsageRecord, 0, len(scan.Records))
	for _, record := range scan.Records {
		if agents[record.AgentID] {
			records = append(records, record)
		}
	}
	sessions := make([]agentsession.UsageSession, 0, len(scan.Sessions))
	for _, session := range scan.Sessions {
		if agents[session.AgentID] {
			sessions = append(sessions, session)
		}
	}
	scan.Records = records
	scan.Sessions = sessions
	return scan
}

func filterSummaries(summaries []agentsession.AgentUsageSummary, agents map[string]bool) []agentsession.AgentUsageSummary {
	if len(agents) == 0 {
		return summaries
	}
	out := make([]agentsession.AgentUsageSummary, 0, len(summaries))
	for _, summary := range summaries {
		if agents[summary.AgentID] {
			out = append(out, summary)
		}
	}
	return out
}

func encode(file *os.File, payload any, pretty bool) error {
	if pretty {
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		_, err = file.Write(data)
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(payload)
}
