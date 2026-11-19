package ingest

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestEstimateCostOneHourWrites(t *testing.T) {
	// A 1-hour cache write is twice the base input price (pricing page, prompt caching).
	for model, rate := range map[string]float64{
		"claude-fable-5-2": 16, "claude-opus-5-5": 8, "claude-sonnet-5-5": 4,
		"claude-opus-4-6": 10, "claude-sonnet-4-6": 6, "claude-haiku-4-5": 2,
	} {
		if got := EstimateCost(model, 0, 0, 0, 1e6, 1e6); math.Abs(got-rate) > 1e-9 {
			t.Errorf("%s: got %v want %v", model, got, rate)
		}
	}
	if got, want := EstimateCost("claude-opus-5-5", 0, 0, 0, 1e6, 250_000), 0.75*5+0.25*8; math.Abs(got-want) > 1e-9 {
		t.Errorf("mixed write: got %v want %v", got, want)
	}
	if got, want := EstimateCost("claude-opus-5-5", 0, 0, 0, 10, 25), 10*8/1e6; math.Abs(got-want) > 1e-15 {
		t.Errorf("1-hour share capped at the write: got %v want %v", got, want)
	}
}

// writeSession writes one project with the given files (relative path -> lines).
func writeSession(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, "-Users-someone-project", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func record(uuid, msgID, reqID string, out int64) string {
	return `{"type":"assistant","sessionId":"s1","uuid":"` + uuid + `","timestamp":"2026-11-19T10:00:00Z","requestId":"` + reqID +
		`","cwd":"/Users/someone/project","message":{"id":"` + msgID + `","model":"claude-opus-5-5","content":[{"type":"text","text":"y"}],` +
		`"usage":{"input_tokens":10,"output_tokens":` + itoa(out) + `,"cache_read_input_tokens":1000000,"cache_creation_input_tokens":0}}}` + "\n"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func onlyGroup(t *testing.T, cyc *Cycle) Group {
	t.Helper()
	if len(cyc.Groups) != 1 {
		t.Fatalf("groups: got %d want 1", len(cyc.Groups))
	}
	return cyc.Groups[0]
}

func sumRows(msgs []Message) (cacheRead int64, cost float64) {
	for _, m := range msgs {
		cacheRead += m.CacheRead
		cost += m.EstCost
	}
	return
}

func TestCountsASplitResponseOnce(t *testing.T) {
	root := writeSession(t, map[string]string{"s1.jsonl": record("u-1", "msg_1", "req_1", 100) + record("u-2", "msg_1", "req_1", 100)})
	g := onlyGroup(t, NewCollector(root, false).Collect(func(string) int64 { return 0 }))
	want := EstimateCost("claude-opus-5-5", 10, 100, 1_000_000, 0, 0)
	if g.Session.CacheRead != 1_000_000 || math.Abs(g.Session.TotalCost-want) > 1e-12 {
		t.Errorf("session: cache read %d cost %v, want 1000000 and %v", g.Session.CacheRead, g.Session.TotalCost, want)
	}
	if cr, cost := sumRows(g.Messages); len(g.Messages) != 2 || cr != 1_000_000 || math.Abs(cost-want) > 1e-12 {
		t.Errorf("rows: %d rows, cache read %d, cost %v", len(g.Messages), cr, cost)
	}
}

func TestPartialOutputOnAnEarlierRecord(t *testing.T) {
	root := writeSession(t, map[string]string{"s1.jsonl": record("u-1", "msg_1", "req_1", 1) + record("u-2", "msg_1", "req_1", 281)})
	g := onlyGroup(t, NewCollector(root, false).Collect(func(string) int64 { return 0 }))
	if g.Session.OutputTokens != 281 || g.Messages[0].OutputTokens != 1 || g.Messages[1].OutputTokens != 280 {
		t.Errorf("output: session %d, rows %d and %d; want 281, 1 and 280",
			g.Session.OutputTokens, g.Messages[0].OutputTokens, g.Messages[1].OutputTokens)
	}
}

func TestCountsARecordCopiedIntoASubagentFileOnce(t *testing.T) {
	line := record("u-1", "msg_1", "req_1", 100)
	root := writeSession(t, map[string]string{"s1.jsonl": line, "s1/subagents/agent-a.jsonl": line})
	g := onlyGroup(t, NewCollector(root, false).Collect(func(string) int64 { return 0 }))
	if g.Session.CacheRead != 1_000_000 || g.Session.MessageCount != 1 || len(g.Messages) != 1 {
		t.Errorf("session cache read %d, count %d, rows %d; want 1000000, 1, 1", g.Session.CacheRead, g.Session.MessageCount, len(g.Messages))
	}
}

func TestSecondRecordInALaterCycleCarriesOnlyTheIncrease(t *testing.T) {
	root := writeSession(t, map[string]string{"s1.jsonl": record("u-1", "msg_1", "req_1", 1)})
	path := filepath.Join(root, "-Users-someone-project", "s1.jsonl")
	c := NewCollector(root, false)
	offsets := map[string]int64{}
	first := c.Collect(func(p string) int64 { return offsets[p] })
	for p, o := range first.Offsets {
		offsets[p] = o
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(record("u-2", "msg_1", "req_1", 281))
	f.Close()
	g := onlyGroup(t, c.Collect(func(p string) int64 { return offsets[p] }))
	if len(g.Messages) != 1 || g.Messages[0].OutputTokens != 280 || g.Messages[0].CacheRead != 0 || g.Session.OutputTokens != 281 {
		t.Errorf("second cycle: rows %d, row output %d, row cache read %d, session output %d",
			len(g.Messages), g.Messages[0].OutputTokens, g.Messages[0].CacheRead, g.Session.OutputTokens)
	}
}

func TestUnacknowledgedRowsAreResentUnchanged(t *testing.T) {
	root := writeSession(t, map[string]string{"s1.jsonl": record("u-1", "msg_1", "req_1", 100) + record("u-2", "msg_1", "req_1", 100)})
	c := NewCollector(root, false)
	first := onlyGroup(t, c.Collect(func(string) int64 { return 0 }))
	again := onlyGroup(t, c.Collect(func(string) int64 { return 0 })) // nothing acknowledged
	if len(again.Messages) != len(first.Messages) {
		t.Fatalf("resend: %d rows, first send %d", len(again.Messages), len(first.Messages))
	}
	for i := range first.Messages {
		if again.Messages[i] != first.Messages[i] {
			t.Errorf("row %d changed on resend: %+v then %+v", i, first.Messages[i], again.Messages[i])
		}
	}
	if again.Session.CacheRead != 1_000_000 {
		t.Errorf("session cache read on resend: %d", again.Session.CacheRead)
	}
}

func TestRemovedFileRebuildsTheProject(t *testing.T) {
	line := record("u-1", "msg_1", "req_1", 100)
	root := writeSession(t, map[string]string{"s1/subagents/agent-a.jsonl": line, "s1.jsonl": line + record("u-9", "msg_9", "req_9", 5)})
	c := NewCollector(root, false)
	c.Collect(func(string) int64 { return 0 })
	// The subagent file, which sorts first and so counted u-1, goes away.
	if err := os.RemoveAll(filepath.Join(root, "-Users-someone-project", "s1")); err != nil {
		t.Fatal(err)
	}
	g := onlyGroup(t, c.Collect(func(string) int64 { return 0 }))
	if g.Session.CacheRead != 2_000_000 || g.Session.MessageCount != 2 {
		t.Errorf("after removal: cache read %d, count %d; want 2000000 and 2 (u-1 is still in s1.jsonl)", g.Session.CacheRead, g.Session.MessageCount)
	}
}
