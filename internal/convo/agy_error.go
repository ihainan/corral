package convo

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AgyErrorStep is a step_type = 17 (ERROR_MESSAGE) row read from Antigravity
// CLI's per-conversation SQLite database (~/.gemini/antigravity-cli/conversations/<sid>.db).
// Antigravity omits ERROR_MESSAGE steps from transcript_full.jsonl, so without
// reading them from the DB, quota exhaustion, rate limits, auth/network/5xx
// errors, and safety refusals would leave the chat view silent.
type AgyErrorStep struct {
	Idx           int
	Status        int
	CreatedAt     int64   // Unix seconds (f5.f1.f1), 0 if absent
	Message       string  // f24.f3.f1: user-facing message
	Detail        string  // f24.f3.f2: full error text
	ErrorID       string  // f24.f3.f6: provider / trajectory error id
	HTTPStatus    int     // f24.f3.f7: HTTP status code (e.g. 429, 500, 503)
	Reason        string  // f24.f3.f10 google.rpc.ErrorInfo.reason
	RetryDelaySec float64 // f24.f3.f10 google.rpc.RetryInfo.retryDelay in seconds
}

// DecodeAgyErrorStep decodes the protobuf wire payload of an agy steps row
// (step_type = 17) without external dependencies. Malformed or truncated input
// is tolerated and returns false only when no error fields could be extracted.
func DecodeAgyErrorStep(idx int, payload []byte) (AgyErrorStep, bool) {
	out := AgyErrorStep{Idx: idx}
	pbWalk(payload, func(fn uint64, wt uint8, v uint64, b []byte) {
		switch fn {
		case 4:
			if wt == 0 {
				out.Status = int(v)
			}
		case 5:
			if wt == 2 {
				pbWalk(b, func(fn2 uint64, wt2 uint8, _ uint64, b2 []byte) {
					if fn2 == 1 && wt2 == 2 {
						pbWalk(b2, func(fn3 uint64, wt3 uint8, v3 uint64, _ []byte) {
							if fn3 == 1 && wt3 == 0 {
								out.CreatedAt = int64(v3)
							}
						})
					}
				})
			}
		case 24:
			if wt == 2 {
				pbWalk(b, func(fn2 uint64, wt2 uint8, _ uint64, b2 []byte) {
					if fn2 == 3 && wt2 == 2 {
						pbWalk(b2, func(fn3 uint64, wt3 uint8, v3 uint64, b3 []byte) {
							switch {
							case fn3 == 1 && wt3 == 2 && out.Message == "":
								out.Message = strings.TrimSpace(string(b3))
							case fn3 == 2 && wt3 == 2 && out.Detail == "":
								out.Detail = strings.TrimSpace(string(b3))
							case fn3 == 6 && wt3 == 2 && out.ErrorID == "":
								out.ErrorID = strings.TrimSpace(string(b3))
							case fn3 == 7 && wt3 == 0 && out.HTTPStatus == 0:
								out.HTTPStatus = int(v3)
							case fn3 == 10 && wt3 == 2:
								agyParseRPCDetail(b3, &out)
							}
						})
					}
				})
			}
		}
	})
	if out.Message == "" && out.Detail == "" && out.HTTPStatus == 0 {
		return AgyErrorStep{}, false
	}
	return out, true
}

func agyParseRPCDetail(raw []byte, out *AgyErrorStep) {
	var d struct {
		Type       string `json:"@type"`
		Reason     string `json:"reason"`
		RetryDelay string `json:"retryDelay"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return
	}
	if d.Reason != "" && out.Reason == "" {
		out.Reason = strings.TrimSpace(d.Reason)
	}
	if d.RetryDelay != "" && out.RetryDelaySec <= 0 {
		s := strings.TrimSpace(d.RetryDelay)
		if strings.HasSuffix(s, "s") {
			if sec, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64); err == nil && sec > 0 && !math.IsNaN(sec) && !math.IsInf(sec, 0) {
				out.RetryDelaySec = sec
			}
		}
	}
}

// pbWalk iterates top-level protobuf wire fields of data, stopping cleanly on
// any truncated or invalid wire record.
func pbWalk(data []byte, fn func(field uint64, wt uint8, varint uint64, bytes []byte)) {
	i := 0
	for i < len(data) {
		tag, n := pbVarint(data[i:])
		if n <= 0 {
			return
		}
		i += n
		field := tag >> 3
		wt := uint8(tag & 7)
		switch wt {
		case 0:
			v, m := pbVarint(data[i:])
			if m <= 0 {
				return
			}
			i += m
			fn(field, wt, v, nil)
		case 1:
			if i+8 > len(data) {
				return
			}
			i += 8
		case 2:
			l, m := pbVarint(data[i:])
			if m <= 0 || l > uint64(len(data)-i-m) {
				return
			}
			i += m
			chunk := data[i : i+int(l)]
			i += int(l)
			fn(field, wt, 0, chunk)
		case 5:
			if i+4 > len(data) {
				return
			}
			i += 4
		default:
			return
		}
	}
}

func pbVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		v |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

var (
	agyCodeRe     = regexp.MustCompile(`(?i)\b(?:code|status|http)\s*[:=]?\s*(\d{3})\b`)
	agyResetsInRe = regexp.MustCompile(`(?i)\b(?:resets?|try again|retry)\s+in\s+((?:\d+\s*d)?\s*(?:\d+\s*h)?\s*(?:\d+\s*m)?\s*(?:\d+(?:\.\d+)?\s*s)?)`)
	agyDurPartRe  = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(ms|[dhms])`)
)

func agyParseResetsIn(s string) (time.Duration, string, bool) {
	m := agyResetsInRe.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	raw := strings.TrimSpace(m[1])
	if raw == "" {
		return 0, "", false
	}
	parts := agyDurPartRe.FindAllStringSubmatch(raw, -1)
	if len(parts) == 0 {
		return 0, "", false
	}
	var total float64
	for _, p := range parts {
		n, err := strconv.ParseFloat(p[1], 64)
		if err != nil || n < 0 {
			continue
		}
		switch strings.ToLower(p[2]) {
		case "d":
			total += n * 86400
		case "h":
			total += n * 3600
		case "m":
			total += n * 60
		case "s":
			total += n
		}
	}
	if total <= 0 {
		return 0, "", false
	}
	return time.Duration(total * float64(time.Second)), raw, true
}

func agyExtractHTTPStatus(s string) int {
	if m := agyCodeRe.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 100 && n <= 599 {
			return n
		}
	}
	return 0
}

// agyClassifyTrouble turns an AGY error (from conversations/<sid>.db or a
// JSONL row) into a canonical Trouble for the conversation view.
func agyClassifyTrouble(msg, detail, reason, errID string, httpStatus int, createdAt int64, retryDelaySec float64) *Trouble {
	msg = strings.TrimSpace(msg)
	detail = strings.TrimSpace(detail)
	if msg == "" {
		msg = detail
	}
	if httpStatus < 100 || httpStatus > 599 {
		httpStatus = agyExtractHTTPStatus(detail)
		if httpStatus == 0 {
			httpStatus = agyExtractHTTPStatus(msg)
		}
	}
	if msg == "" && httpStatus == 0 {
		return nil
	}
	full := msg
	if errID != "" && !strings.Contains(full, errID) {
		if full != "" {
			full += "\nError ID: " + clip(errID, maxMeta)
		} else {
			full = "Error ID: " + clip(errID, maxMeta)
		}
	}
	said, cutOff := cut(clean(full), maxTrouble)
	reasonUp := strings.ToUpper(strings.TrimSpace(reason))
	combined := strings.ToLower(msg + " " + detail)

	// 1. Subscription quota / rate limit exhaustion.
	isLimit := httpStatus == 429 ||
		reasonUp == "QUOTA_EXHAUSTED" ||
		reasonUp == "RATE_LIMIT_EXCEEDED" ||
		reasonUp == "MODEL_CAPACITY_EXHAUSTED" ||
		strings.Contains(combined, "quota reached") ||
		strings.Contains(combined, "quota exceeded") ||
		strings.Contains(combined, "usage limit") ||
		strings.Contains(combined, "rate limit") ||
		strings.Contains(combined, "resource_exhausted")
	if isLimit {
		tr := &Trouble{Kind: TroubleLimit, Text: said, Status: httpStatus, Cut: cutOff}
		if createdAt > 0 && retryDelaySec > 0 {
			tr.ResetsAt = createdAt + int64(math.Round(retryDelaySec))
		} else if d, raw, ok := agyParseResetsIn(msg + " " + detail); ok {
			if createdAt > 0 {
				tr.ResetsAt = createdAt + int64(math.Round(d.Seconds()))
			} else {
				tr.Until = clip(raw, 40)
			}
		}
		return tr
	}

	// 2. Safety / content policy refusal.
	isRefused := strings.Contains(reasonUp, "SAFETY") ||
		strings.Contains(reasonUp, "RECITATION") ||
		strings.Contains(reasonUp, "BLOCKLIST") ||
		strings.Contains(reasonUp, "PROHIBITED_CONTENT") ||
		strings.Contains(reasonUp, "POLICY") ||
		strings.Contains(combined, "safety filter") ||
		strings.Contains(combined, "blocked by safety") ||
		strings.Contains(combined, "content policy") ||
		strings.Contains(combined, "prohibited content") ||
		strings.Contains(combined, "safeguards")
	if isRefused {
		return &Trouble{Kind: TroubleRefused, Text: said, Status: httpStatus, Cut: cutOff}
	}

	// 3. Model output length limit.
	if reasonUp == "MAX_TOKENS" {
		return &Trouble{Kind: TroubleCut, Text: said, Cut: cutOff}
	}

	// 4. All other API / auth / network / context / server errors.
	return &Trouble{Kind: TroubleAPI, Text: said, Status: httpStatus, Cut: cutOff}
}

func agyEntryStepIndex(id string) int {
	rest, ok := strings.CutPrefix(id, "step-")
	if !ok {
		return 0
	}
	if dash := strings.IndexByte(rest, '-'); dash >= 0 {
		rest = rest[:dash]
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// MinStepIndex returns the lowest step_index among currently retained entries
// (0 if no entries are retained yet).
func (c *Agy) MinStepIndex() int {
	minIdx := 0
	for _, e := range c.entries {
		if idx := agyEntryStepIndex(e.ID); idx > 0 && (minIdx == 0 || idx < minIdx) {
			minIdx = idx
		}
	}
	return minIdx
}

// MergeDBErrors integrates ERROR_MESSAGE steps read from conversations/<sid>.db
// into the conversation in chronological step_index order.
//
// added is true when at least one new error entry was inserted or appended.
// spliced is true when an entry was inserted before an existing entry (so the
// caller should bump gen / trigger a reset to deliver the reordered sequence).
func (c *Agy) MergeDBErrors(errs []AgyErrorStep) (added bool, spliced bool) {
	if len(errs) == 0 {
		return false, false
	}
	sorted := append([]AgyErrorStep(nil), errs...)
	slices.SortFunc(sorted, func(a, b AgyErrorStep) int { return a.Idx - b.Idx })

	minIdx := c.MinStepIndex()
	bumpedRev := false
	for _, e := range sorted {
		if e.Idx <= 0 || c.seenDBErrs[e.Idx] {
			continue
		}
		if minIdx > 0 && e.Idx < minIdx {
			// Older than the loaded tail window; keep unseen so readOlder can include it.
			continue
		}
		if c.seenDBErrs == nil {
			c.seenDBErrs = map[int]bool{}
		}
		c.seenDBErrs[e.Idx] = true

		id := fmt.Sprintf("step-%d", e.Idx)
		exists := false
		for _, cur := range c.entries {
			if cur.ID == id {
				exists = true
				break
			}
		}
		if exists {
			continue
		}

		tr := agyClassifyTrouble(e.Message, e.Detail, e.Reason, e.ErrorID, e.HTTPStatus, e.CreatedAt, e.RetryDelaySec)
		if tr == nil {
			continue
		}
		ts := ""
		if e.CreatedAt > 0 {
			ts = time.Unix(e.CreatedAt, 0).UTC().Format(time.RFC3339)
		}
		ent := &Entry{
			ID:    id,
			Role:  RoleNote,
			Time:  ts,
			Parts: []Part{{Kind: KindTrouble, Trouble: tr}},
		}

		if !bumpedRev {
			c.rev++
			bumpedRev = true
		}

		pos := len(c.entries)
		for i, cur := range c.entries {
			if curIdx := agyEntryStepIndex(cur.ID); curIdx > e.Idx {
				pos = i
				break
			}
		}
		if pos == len(c.entries) {
			c.appendEntry(ent)
		} else {
			c.entries = slices.Insert(c.entries, pos, ent)
			spliced = true
		}
		added = true

		if e.Idx >= c.maxStep {
			c.maxStep = e.Idx
			c.pending = nil
			c.working, c.known = false, true
		}
	}

	if spliced && len(c.entries) > 0 {
		base := c.entries[0].Seq
		if base <= 0 {
			base = 1
		}
		for i, e := range c.entries {
			e.Seq = base + i
			e.Rev = c.rev
		}
		c.seq = c.entries[len(c.entries)-1].Seq
	}
	if added {
		c.trim()
	}
	return added, spliced
}

// EndedOnHost removes background tasks that the host process scan proves are
// no longer running, mirroring Codex.EndedOnHost.
func (c *Agy) EndedOnHost(gone func(Running) bool) {
	var keep []string
	for _, id := range c.runOrder {
		r, ok := c.running[id]
		if !ok {
			continue
		}
		if gone(r) {
			delete(c.running, id)
			continue
		}
		keep = append(keep, id)
	}
	c.runOrder = keep
}
