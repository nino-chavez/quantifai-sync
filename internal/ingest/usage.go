package ingest

import (
	"bytes"
	"encoding/json"
	"strings"
)

// pricingRates is one set of list prices per 1M tokens. cacheCreation is the
// 5-minute cache-write rate.
type pricingRates struct {
	input, output float64
	cacheRead     float64
	cacheCreation float64
}

// pricingRow is one tier of the server's ANTHROPIC_PRICING_TABLE
// (apps/app/src/lib/pricing/anthropic-pricing.ts).
type pricingRow struct {
	match string
	pricingRates
	// longPromptOver > 0 means a request whose prompt (input + cache read +
	// cache creation) exceeds it pays longPrompt for every category.
	longPromptOver int64
	longPrompt     pricingRates
}

func row(match string, in, out, cacheRead, cacheCreation float64) pricingRow {
	return pricingRow{match: match, pricingRates: pricingRates{in, out, cacheRead, cacheCreation}}
}

// First match wins, by substring on the lowercased model, so a version comes
// before any key it contains. The bare family rows at the end catch older
// versions at legacy rates. Unknown models fall back to the sonnet row, as
// the server does. Keep this in the server table's order and values.
var pricingTable = []pricingRow{
	row("fable-5-1", 10, 50, 0.25, 12.5),
	row("mythos-5-1", 10, 50, 0.25, 12.5),
	row("fable-5", 10, 50, 1, 12.5),
	row("mythos-5", 10, 50, 1, 12.5),
	row("opus-5-5", 4, 20, 0.2, 5),
	row("opus-5", 5, 25, 0.5, 6.25),
	row("opus-4-8", 5, 25, 0.5, 6.25),
	row("opus-4-7", 5, 25, 0.5, 6.25),
	row("opus-4-6", 5, 25, 0.5, 6.25),
	row("opus-4-5", 5, 25, 0.5, 6.25),
	row("opus-4", 15, 75, 1.5, 18.75),
	row("sonnet-5-5", 2, 10, 0.2, 2.5),
	row("sonnet-5", 2, 10, 0.2, 2.5),
	{
		match:          "haiku-5-5",
		pricingRates:   pricingRates{0.1, 0.5, 0.01, 0.125},
		longPromptOver: 100_000,
		longPrompt:     pricingRates{0.5, 2.5, 0.05, 0.625},
	},
	row("haiku-4-5", 1, 5, 0.1, 1.25),
	row("opus", 15, 75, 1.5, 18.75),
	row("haiku", 0.8, 4, 0.08, 1),
	row("sonnet", 3, 15, 0.3, 3.75),
}

var fallbackRow = pricingTable[len(pricingTable)-1]

// EstimateCost ports estimateAnthropicCost. Each term is converted with
// float64() so the compiler cannot fuse it into a multiply-add on arm64;
// a fused result differs from JavaScript's in the last bits, and the rows
// must match the reference importer's exactly.
func EstimateCost(model string, in, out, cacheRead, cacheCreation int64) float64 {
	m := strings.ToLower(model)
	tier := fallbackRow
	for _, r := range pricingTable {
		if strings.Contains(m, r.match) {
			tier = r
			break
		}
	}
	rates := tier.pricingRates
	if tier.longPromptOver > 0 && in+cacheRead+cacheCreation > tier.longPromptOver {
		rates = tier.longPrompt
	}
	t1 := float64(float64(in) / 1_000_000 * rates.input)
	t2 := float64(float64(out) / 1_000_000 * rates.output)
	t3 := float64(float64(cacheRead) / 1_000_000 * rates.cacheRead)
	t4 := float64(float64(cacheCreation) / 1_000_000 * rates.cacheCreation)
	return t1 + t2 + t3 + t4
}

// Usage is one cost-bearing assistant record (server: UsageMessage).
type Usage struct {
	SessionID     string
	MessageID     string // the record's uuid, not message.id
	Timestamp     string
	Model         string
	Cwd           string // "" when absent
	Editor        string // entrypoint; "" when absent
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	CacheCreation int64
	CostUSD       float64
	ToolNames     []string
}

type rawRecord struct {
	Type       string  `json:"type"`
	SessionID  string  `json:"sessionId"`
	UUID       string  `json:"uuid"`
	Timestamp  string  `json:"timestamp"`
	Cwd        *string `json:"cwd"`
	Entrypoint *string `json:"entrypoint"`
	Message    *struct {
		Model   *string         `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens   int64 `json:"input_tokens"`
			OutputTokens  int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var usageKey = []byte(`"usage"`)

// ExtractUsage ports extractUsageMessage: it returns nil for anything that
// is not an assistant record with a usage block and a sessionId, uuid and
// timestamp. Most lines in a session file are user records and tool
// output, so lines without a "usage" key are skipped before decoding.
func ExtractUsage(line []byte) *Usage {
	if !bytes.Contains(line, usageKey) {
		return nil
	}
	var r rawRecord
	if err := json.Unmarshal(line, &r); err != nil {
		return nil
	}
	if r.Type != "assistant" || r.SessionID == "" || r.UUID == "" || r.Timestamp == "" {
		return nil
	}
	if r.Message == nil || r.Message.Usage == nil {
		return nil
	}
	u := r.Message.Usage

	model := "unknown"
	if r.Message.Model != nil {
		model = *r.Message.Model
	}
	msg := &Usage{
		SessionID:     r.SessionID,
		MessageID:     r.UUID,
		Timestamp:     r.Timestamp,
		Model:         model,
		InputTokens:   u.InputTokens,
		OutputTokens:  u.OutputTokens,
		CacheRead:     u.CacheRead,
		CacheCreation: u.CacheCreation,
		CostUSD:       EstimateCost(model, u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheCreation),
		ToolNames:     toolNames(r.Message.Content),
	}
	if r.Cwd != nil {
		msg.Cwd = *r.Cwd
	}
	if r.Entrypoint != nil {
		msg.Editor = *r.Entrypoint
	}
	return msg
}

// toolNames collects the name of every tool_use block, in order,
// duplicates included (the accumulator dedups).
func toolNames(content json.RawMessage) []string {
	var blocks []struct {
		Type string          `json:"type"`
		Name json.RawMessage `json:"name"`
	}
	if len(content) == 0 || json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var names []string
	for _, b := range blocks {
		// Only string names count (the server checks typeof name === 'string').
		if b.Type != "tool_use" || len(b.Name) == 0 || b.Name[0] != '"' {
			continue
		}
		var name string
		if json.Unmarshal(b.Name, &name) == nil {
			names = append(names, name)
		}
	}
	return names
}

// MessageRow converts a usage record to the messages row the reference
// importer writes for it.
func (u *Usage) MessageRow() Message {
	return Message{
		SessionID:      u.SessionID,
		MessageID:      u.MessageID,
		Timestamp:      u.Timestamp,
		Model:          u.Model,
		Provider:       "anthropic",
		InputTokens:    u.InputTokens,
		OutputTokens:   u.OutputTokens,
		CacheRead:      u.CacheRead,
		CacheCreation:  u.CacheCreation,
		EstCost:        u.CostUSD,
		CostProvenance: "estimated",
		RecordType:     nil,
	}
}
