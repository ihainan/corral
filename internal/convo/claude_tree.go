package convo

// What a rewind took back, in Claude Code's log.
//
// Each row of the conversation names the row it follows (uuid,
// parentUuid; logicalParentUuid across a compaction): the log is a tree.
// Most of its forks are of calls made together, the next call and the
// result of the one before both hanging on that one: all of it stands. A
// rewind is the other kind of fork. The person's next message hangs on a
// row from before, with rows already written below that one: those are
// what the conversation went back from, and the agent no longer has them.
// Esc Esc and /rewind do so; so does a message taken back by Esc as soon
// as it was sent. (Of 7814 messages in 252 of this machine's sessions, 138
// hung so; every other one hung on the row written just before it.) What
// Claude Code writes itself (isMeta) is not taken for one: /exit's caveat
// hangs where the message before the last does, and the session, resumed,
// goes on from the last.
//
// What was taken back is known when that next message comes, not before:
// a message taken back as it went, with nothing sent since, still shows.
// An answer the model's safeguards stopped, answered again by another
// model, is withdrawn by name (retractedMessageUuids).

// rowTree holds, of the rows read (up to maxTreeRows of the latest), what
// each hangs on and its place (ord) among them.
type rowTree struct {
	at   map[string]int // uuid → ord
	up   []string       // the row each hangs on, by ord - base ("" for none)
	ids  []string       // each row's uuid, by ord - base
	base int            // the ord of the first kept
	next int            // the ord the next row takes
	cur  int            // the ord of the row being read (-1: none)
	// curRoot: it says it hangs on none (parentUuid null, not absent: a
	// log written without them says nothing of where a row hangs).
	curRoot bool
	lost bool           // rows were dropped as old: one not here may be among them
	// cuts: rewinds to a row in a part of the log before the one read,
	// to be told once that part is put before (see Prepend).
	cuts []treeCut
	// withdrawn: answers withdrawn whose rows were not read: of that part.
	withdrawn map[string]bool
	// again: for a row the conversation went back to, where the last such
	// message was: what hung below it before that was taken back then, so
	// going back to it again (and again) looks only from there on.
	again map[string]int
	// dead: the rows taken back; a row read later hanging on one of them
	// (a result come late, of a call taken back) is taken back with it.
	dead map[string]bool
}

// cut keeps a rewind to a row not read yet. One to a row already kept
// for is that one, from the latest message going back to it: what it
// takes back takes in the earlier ones'.
func (t *rowTree) cut(parent string, at int) {
	for i, k := range t.cuts {
		if k.parent == parent {
			t.cuts[i].at = max(k.at, at)
			return
		}
	}
	if len(t.cuts) < maxTreeCuts {
		t.cuts = append(t.cuts, treeCut{parent, at})
	}
}

type treeCut struct {
	parent string
	at     int // the ord of the message that went back
}

const (
	maxTreeRows  = 60000
	maxTreeCuts  = 64
	maxWithdrawn = 256
)

// add reads in a row: its uuid and what it hangs on. A uuid read again
// keeps its first place.
func (t *rowTree) add(id, parent string) {
	t.cur = -1
	if id == "" {
		return
	}
	if t.at == nil {
		t.at = map[string]int{}
	}
	if o, ok := t.at[id]; ok {
		t.cur = o
		return
	}
	if t.dead[parent] {
		t.dead[id] = true
	}
	t.at[id] = t.next
	t.ids = append(t.ids, id)
	t.up = append(t.up, parent)
	t.cur = t.next
	t.next++
	if len(t.ids) > maxTreeRows {
		t.drop(len(t.ids) - maxTreeRows + maxTreeRows/6) // (a sixth at a time: not at every row)
	}
}

// drop forgets the n oldest rows.
func (t *rowTree) drop(n int) {
	n = min(n, len(t.ids))
	for _, id := range t.ids[:n] {
		delete(t.at, id)
		delete(t.dead, id)
	}
	t.ids = append([]string(nil), t.ids[n:]...)
	t.up = append([]string(nil), t.up[n:]...)
	t.base += n
	t.lost = true
}

// below: the rows read between parent and the row at ord before that hang
// (at any depth) on parent, from ord from on. One pass: a row's parent
// comes before it. parent "": the conversation went back to its start (a
// message hanging on none): every row before.
func (t *rowTree) below(parent string, from, before int) map[string]bool {
	out := map[string]bool{}
	if parent == "" {
		for o := max(from, t.base); o < before && o < t.next; o++ {
			if id := t.ids[o-t.base]; id != "" {
				out[id] = true
			}
		}
		return out
	}
	p, ok := t.at[parent]
	if !ok {
		return nil
	}
	in := map[string]bool{parent: true}
	for o := max(p+1, from); o < before && o < t.next; o++ {
		i := o - t.base
		// (Hanging on a row taken back before: one gone back from already,
		// below it, or of another rewind.)
		if id := t.ids[i]; id != "" && (in[t.up[i]] || t.dead[t.up[i]]) { // (a row read twice keeps its first place: see treeBefore)
			in[id] = true
			out[id] = true
		}
	}
	return out
}

// rewound: the row being read is a message of the person's. Where rows
// were read below the row it hangs on, before it, the
// conversation went back: they are taken back. Where that row is not
// read (in the part before), that is told when the part is put before.
func (c *Claude) rewound() {
	t := &c.tree
	if t.cur < 0 {
		return
	}
	parent := t.up[t.cur-t.base]
	if parent == "" && !t.curRoot {
		return
	}
	if parent == "" {
		// (A message hanging on none: the first of the session, or one sent
		// after going back to its start. Of 252 sessions here, none of the
		// latter; but then all before it is taken back, the part before too.)
		if t.cur > t.base {
			c.takeBack(c.entries, t.below("", t.base, t.cur))
		}
		if !t.lost {
			t.cut("", t.cur)
		}
		return
	}
	p, ok := t.at[parent]
	if !ok {
		if !t.lost {
			t.cut(parent, t.cur)
		}
		return
	}
	if p == t.cur-1 { // (the row just before: as nearly always)
		return
	}
	from, seen := t.again[parent]
	if !seen {
		from = p + 1
	}
	c.takeBack(c.entries, t.below(parent, from, t.cur))
	if t.again == nil {
		t.again = map[string]int{}
	}
	if len(t.again) < maxTreeCuts || seen {
		t.again[parent] = t.cur
	}
}

// withdraw takes back the answers the model's safeguards withdrew, by
// their rows' uuids; those not read are kept to be told in the part before.
func (c *Claude) withdraw(ids []string) {
	want := map[string]bool{}
	for _, id := range ids {
		if id = clip(id, maxID); id != "" {
			want[id] = true
		}
	}
	// (Not taken back as a branch is: the answer given again hangs on the
	// one withdrawn, through Claude Code's row saying so.)
	for _, e := range goneMark(c.entries, want) {
		e.Rev = c.rev
	}
	t := &c.tree
	for id := range want {
		if _, read := t.at[id]; read || t.lost {
			continue
		}
		if t.withdrawn == nil {
			t.withdrawn = map[string]bool{}
		}
		if len(t.withdrawn) < maxWithdrawn {
			t.withdrawn[id] = true
		}
	}
}

// takeBack marks the entries of these rows gone, as of this revision, and
// the rows themselves taken back.
func (c *Claude) takeBack(entries []*Entry, ids map[string]bool) {
	c.tree.kill(ids)
	for _, e := range goneMark(entries, ids) {
		e.Rev = c.rev
	}
}

// kill notes these rows taken back (those read).
func (t *rowTree) kill(ids map[string]bool) {
	for id := range ids {
		if _, read := t.at[id]; read {
			if t.dead == nil {
				t.dead = map[string]bool{}
			}
			t.dead[id] = true
		}
	}
}

// treeBefore puts o's tree before c's (o read the part of the log before
// c's), then tells what c's rewinds and withdrawals into that part took
// back: of o's entries, and of c's own (a rewind's message in c's part to
// a row in o's takes back what hangs below it in both). It marks them
// gone without setting Rev (Prepend sets it, as it does for what it puts
// before) and gives those of c's own that it marked.
func (c *Claude) treeBefore(o *Claude) []*Entry {
	t, u := &c.tree, &o.tree
	shift := t.base - u.next // (o's rows end where c's begin)
	if t.at == nil {
		t.at = map[string]int{}
	}
	ids := make([]string, 0, len(u.ids)+len(t.ids))
	up := make([]string, 0, len(u.ids)+len(t.ids))
	for i, id := range u.ids {
		if _, dup := t.at[id]; dup {
			// (Read again later: its place is that one. A message in c's part
			// hanging on such a row is not told again as a rewind: no log of
			// this machine's writes a uuid twice.)
			id = ""
		} else {
			t.at[id] = u.base + i + shift
		}
		ids = append(ids, id)
		up = append(up, u.up[i])
	}
	t.ids = append(ids, t.ids...)
	t.up = append(up, t.up...)
	t.base = u.base + shift
	t.lost = t.lost || u.lost
	for id := range u.dead {
		if t.dead == nil {
			t.dead = map[string]bool{}
		}
		t.dead[id] = true
	}
	// (A row o took back read again in c's part: its entries there too.)
	mine := goneMark(c.entries, u.dead)
	if len(t.ids) > maxTreeRows {
		// (The oldest go: they are o's, before anything a cut of c's names.)
		n := len(t.ids) - maxTreeRows
		for _, id := range t.ids[:n] {
			if id != "" {
				delete(t.at, id)
			}
		}
		t.ids, t.up = t.ids[n:], t.up[n:]
		t.base += n
		t.lost = true
	}
	var keep []treeCut
	for _, k := range t.cuts {
		if _, ok := t.at[k.parent]; ok || k.parent == "" {
			below := t.below(k.parent, t.base, k.at)
			t.kill(below)
			goneMark(o.entries, below)
			mine = append(mine, goneMark(c.entries, below)...)
			if k.parent == "" && !t.lost { // (back to the start: a part before that is taken back too)
				keep = append(keep, k)
			}
		} else if !t.lost {
			keep = append(keep, k)
		}
	}
	t.cuts = keep
	for _, k := range u.cuts { // (o's own, to a part before it)
		t.cut(k.parent, k.at+shift)
	}
	if len(t.withdrawn) > 0 {
		goneMark(o.entries, t.withdrawn)
		for id := range t.withdrawn {
			if _, read := t.at[id]; read || t.lost {
				delete(t.withdrawn, id)
			}
		}
	}
	for id := range u.withdrawn {
		if t.withdrawn == nil {
			t.withdrawn = map[string]bool{}
		}
		if len(t.withdrawn) < maxWithdrawn {
			t.withdrawn[id] = true
		}
	}
	// Rows hanging on one taken back (by o's part, or by what was told
	// just now): taken back too, in either part. One pass: a row's parent
	// comes before it.
	var late map[string]bool
	for o := t.base; o < t.next && len(t.dead) > 0; o++ {
		i := o - t.base
		if id := t.ids[i]; id != "" && !t.dead[id] && t.dead[t.up[i]] {
			t.dead[id] = true
			if late == nil {
				late = map[string]bool{}
			}
			late[id] = true
		}
	}
	goneMark(o.entries, late)
	return append(mine, goneMark(c.entries, late)...)
}

// goneMark marks the entries of these rows gone (their Rev is the
// caller's to set), and gives those it marked.
func goneMark(entries []*Entry, ids map[string]bool) []*Entry {
	var out []*Entry
	for _, e := range entries {
		if !e.Gone && ids[e.ID] {
			e.Gone = true
			out = append(out, e)
		}
	}
	return out
}
