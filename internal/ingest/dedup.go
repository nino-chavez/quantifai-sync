package ingest

import "hash/fnv"

// responseUsage is one API response's largest usage seen so far, field by field.
type responseUsage struct {
	in, out, cacheRead, cacheCreation, cacheCreation1h int64
}

// sessionDedup is what one session has already counted, the Go side of the
// reference importer's SessionDedup (apps/app/src/lib/importers/usage-record.ts).
// Keys are 64-bit FNV-1a hashes of the record uuid and the response key, so the
// state stays small for a whole history held in memory.
type sessionDedup struct {
	seen      map[uint64]struct{}
	responses map[uint64]responseUsage
}

func newSessionDedup() *sessionDedup {
	return &sessionDedup{seen: map[uint64]struct{}{}, responses: map[uint64]responseUsage{}}
}

func keyHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// countOnce ports countOnce. Feed it every record of one session in walk order.
//
// A record whose uuid was already counted (the same record copied into a second
// file of the session, such as its subagents folder) returns nil. Otherwise the
// record carries only the increase over the largest usage its response has shown
// so far, field by field, and the cost of that increase. A response's records
// then sum to its true usage, whether every record repeats it or earlier records
// carry a partial output count. Claude Code writes one API response as one record
// per content block, each with its own uuid and the response's usage.
func (d *sessionDedup) countOnce(u *Usage) *Usage {
	id := keyHash(u.MessageID)
	if _, ok := d.seen[id]; ok {
		return nil
	}
	d.seen[id] = struct{}{}

	rk := keyHash(u.ResponseKey)
	prev, had := d.responses[rk]
	next := responseUsage{
		in:              max(prev.in, u.InputTokens),
		out:             max(prev.out, u.OutputTokens),
		cacheRead:       max(prev.cacheRead, u.CacheRead),
		cacheCreation:   max(prev.cacheCreation, u.CacheCreation),
		cacheCreation1h: max(prev.cacheCreation1h, u.CacheCreation1h),
	}
	d.responses[rk] = next
	if !had {
		return u
	}

	c := *u
	c.InputTokens = next.in - prev.in
	c.OutputTokens = next.out - prev.out
	c.CacheRead = next.cacheRead - prev.cacheRead
	c.CacheCreation = next.cacheCreation - prev.cacheCreation
	c.CacheCreation1h = next.cacheCreation1h - prev.cacheCreation1h
	c.CostUSD = EstimateCost(u.Model, next.in, next.out, next.cacheRead, next.cacheCreation, next.cacheCreation1h) -
		EstimateCost(u.Model, prev.in, prev.out, prev.cacheRead, prev.cacheCreation, prev.cacheCreation1h)
	return &c
}
