// agent-session-sdk: CLI over the agentsession library. Scans local
// coding-agent stores and prints session lists, usage, summaries or one
// session's transcript as JSON on stdout. Pure Go dependencies only (no cgo),
// single-file distribution, one cross-compile command away. The session and
// usage shapes stay field-compatible with agent-view's ipc-contract.ts. The
// optional --index path persists a derived session index in SQLite and only
// re-scans agent stores whose file signatures changed.
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
  agent-session-sdk <sessions|usage|summary|transcript|search> [flags]

Subcommands:
  sessions    Session list for all agents (SessionScanResult[] JSON)
  usage       Token usage records for all agents ({records, sessions} JSON)
  summary     Per-agent usage overview (sessions / requests / total tokens / active time)
  transcript  One session's transcript (Entry[] JSON, needs --agent and --session)
  search      Search indexed session titles, workspaces and ids (query first)
  help        Show this help

Flags:
  --home <dir>     Target user home directory (default $HOME)
  --agent <ids>    Only process the given agents, comma-separated (e.g. --agent codex,claude)
  --session <id>   Transcript subcommand only: the session id to load
  --cost           Usage subcommand only: append costUsd / costUnpricedTokens per record
  --index          Sessions/transcript only: use <home>/.agent-session-sdk/home.sqlite
  --no-refresh     Indexed reads only: read SQLite without checking stores
  --limit <n>      Indexed sessions/search only: limit output (0 means unlimited)
  --offset <n>     Indexed sessions/search only: skip leading results
  --pretty         Indent output for readability

Session scanning covers 16 agents: claude codex gemini cursor opencode zcode
workbuddy pi dsh grok qoder qoder-cn kimi kimi-code hermes. Usage / summary cover the
agents with local usage data: claude codex zcode workbuddy dsh pi opencode grok
qoder qoder-cn hermes. Qoder / qoder-cn tokens are heuristically reconstructed
from context ratio + reply length, and records keep credits for cost
estimation — free requests included.

Examples:
  agent-session-sdk sessions --agent codex --pretty
  agent-session-sdk usage --home /Users/demo --cost | jq '.records | length'
  agent-session-sdk summary --agent codex,claude
  agent-session-sdk transcript --agent claude --session <sessionId> --pretty
  agent-session-sdk sessions --index --agent codex --pretty
  agent-session-sdk search 登录 --agent claude --limit 20
`

type cliOptions struct {
	home      string
	agents    map[string]bool
	pretty    bool
	session   string
	cost      bool
	index     bool
	noRefresh bool
	limit     int
	offset    int
	search    string
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
		if opts.index {
			payload = listIndexedSessions(opts)
		} else {
			payload = filterSessions(agentsession.ScanSessions(opts.home), opts.agents)
		}
	case "usage":
		payload = costify(filterUsage(agentsession.ScanAllUsage(opts.home), opts.agents), opts.cost)
	case "summary":
		payload = filterSummaries(agentsession.Summarize(agentsession.ScanAllUsage(opts.home)), opts.agents)
	case "transcript":
		payload = loadTranscript(opts)
	case "search":
		payload = searchIndexedSessions(opts)
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
	searchQuery := ""
	if command == "search" {
		flags := make([]string, 0, len(args))
		valueFlags := map[string]bool{"home": true, "agent": true, "session": true, "limit": true, "offset": true}
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if strings.HasPrefix(arg, "-") {
				flags = append(flags, arg)
				name := strings.TrimLeft(arg, "-")
				if !strings.Contains(arg, "=") && valueFlags[name] {
					if i+1 >= len(args) {
						fmt.Fprintf(os.Stderr, "flag %s needs a value\n", arg)
						os.Exit(2)
					}
					i++
					flags = append(flags, args[i])
				}
				continue
			}
			if searchQuery != "" {
				fmt.Fprintf(os.Stderr, "unknown argument: %s\n", arg)
				os.Exit(2)
			}
			searchQuery = arg
		}
		if searchQuery == "" {
			fmt.Fprintln(os.Stderr, "search needs <query>")
			os.Exit(2)
		}
		args = flags
	}
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, helpText) }
	opts := &cliOptions{agents: map[string]bool{}}
	fs.StringVar(&opts.home, "home", "", "target user home directory (default $HOME)")
	agentList := fs.String("agent", "", "only process the given agents, comma-separated")
	fs.BoolVar(&opts.pretty, "pretty", false, "indent output")
	fs.StringVar(&opts.session, "session", "", "session id (transcript only)")
	fs.BoolVar(&opts.cost, "cost", false, "append cost fields (usage only)")
	fs.BoolVar(&opts.index, "index", false, "use the persistent session index")
	fs.BoolVar(&opts.noRefresh, "no-refresh", false, "read the index without refreshing")
	fs.IntVar(&opts.limit, "limit", 0, "limit indexed results (0 means unlimited)")
	fs.IntVar(&opts.offset, "offset", 0, "skip leading indexed results")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if remaining := fs.Args(); len(remaining) > 0 {
		fmt.Fprintf(os.Stderr, "unknown argument: %s\n", remaining[0])
		os.Exit(2)
	}
	opts.search = searchQuery
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
	if opts.index && command != "sessions" && command != "transcript" {
		fmt.Fprintln(os.Stderr, "--index is valid for sessions and transcript only")
		os.Exit(2)
	}
	if opts.noRefresh && ((command == "sessions" || command == "transcript") && !opts.index) {
		fmt.Fprintln(os.Stderr, "--no-refresh needs --index")
		os.Exit(2)
	}
	if opts.noRefresh && command != "sessions" && command != "search" && command != "transcript" {
		fmt.Fprintln(os.Stderr, "--no-refresh is valid for indexed sessions, search and transcript only")
		os.Exit(2)
	}
	if (command != "search" && (command != "sessions" || !opts.index)) && (opts.limit != 0 || opts.offset != 0) {
		fmt.Fprintln(os.Stderr, "--limit/--offset are valid for indexed sessions and search only")
		os.Exit(2)
	}
	if opts.limit < 0 || opts.offset < 0 {
		fmt.Fprintln(os.Stderr, "--limit/--offset cannot be negative")
		os.Exit(2)
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
// fields on top of the base contract, and the top level carries the price
// table version so consumers can invalidate cost caches without a manual bump.
// Without --cost the plain UsageScan is emitted, byte-identical to the
// previous scanner.
type costScan struct {
	Records        []costRecord                `json:"records"`
	Sessions       []agentsession.UsageSession `json:"sessions"`
	PricingVersion uint32                      `json:"pricingVersion"`
}

func costify(scan agentsession.UsageScan, withCost bool) any {
	if !withCost {
		return scan
	}
	out := costScan{Records: make([]costRecord, 0, len(scan.Records)), Sessions: scan.Sessions, PricingVersion: agentsession.PricingVersion}
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

func selectedAgentIDs(selected map[string]bool) []string {
	if len(selected) == 0 {
		return nil
	}
	ids := make([]string, 0, len(selected))
	for _, id := range agentsession.KnownAgentIDs() {
		if selected[id] {
			ids = append(ids, id)
		}
	}
	return ids
}

func listIndexedSessions(opts *cliOptions) any {
	index, err := agentsession.OpenSessionIndex(opts.home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open session index: %v\n", err)
		return nil
	}
	defer index.Close()
	if !opts.noRefresh {
		if _, err := index.Refresh(selectedAgentIDs(opts.agents)...); err != nil {
			fmt.Fprintf(os.Stderr, "failed to refresh session index: %v\n", err)
			return nil
		}
	}
	results, err := index.List(agentsession.SessionIndexQuery{
		AgentIDs: selectedAgentIDs(opts.agents),
		Limit:    opts.limit,
		Offset:   opts.offset,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to query session index: %v\n", err)
		return nil
	}
	return results
}

func searchIndexedSessions(opts *cliOptions) any {
	index, err := agentsession.OpenSessionIndex(opts.home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open session index: %v\n", err)
		return nil
	}
	defer index.Close()
	if !opts.noRefresh {
		if _, err := index.Refresh(selectedAgentIDs(opts.agents)...); err != nil {
			fmt.Fprintf(os.Stderr, "failed to refresh session index: %v\n", err)
			return nil
		}
	}
	results, err := index.List(agentsession.SessionIndexQuery{
		AgentIDs: selectedAgentIDs(opts.agents),
		Search:   opts.search,
		Limit:    opts.limit,
		Offset:   opts.offset,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to search session index: %v\n", err)
		return nil
	}
	return results
}

func loadTranscript(opts *cliOptions) any {
	var agentID string
	for id := range opts.agents {
		agentID = id
	}
	var info *agentsession.SessionInfo
	if opts.index {
		index, err := agentsession.OpenSessionIndex(opts.home)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open session index: %v\n", err)
			return nil
		}
		if !opts.noRefresh {
			if _, err := index.Refresh(agentID); err != nil {
				index.Close()
				fmt.Fprintf(os.Stderr, "failed to refresh session index: %v\n", err)
				return nil
			}
		}
		info, err = index.Get(agentID, opts.session)
		if err != nil {
			index.Close()
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return nil
		}
		index.Close()
		return transcriptEntries(agentID, *info)
	}
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
	return transcriptEntries(agentID, *info)
}

func transcriptEntries(agentID string, info agentsession.SessionInfo) any {
	entries, err := agentsession.LoadTranscript(agentID, info)
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
