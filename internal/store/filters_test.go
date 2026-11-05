package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Recordings narrowed by owner, host (a deleted host's too: by id) and when
// they started; the choices are the hosts and owners there are recordings
// of, by their names as last recorded, the latest first.
func TestRecordingFilters(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(id string, owner, host int64, hostName string, at int64) {
		t.Helper()
		if err := s.CreateRecording(ctx, &Recording{ID: id, OwnerID: owner, OwnerName: fmt.Sprint("u", owner), HostID: host, HostName: hostName, StartedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	add("a", 1, 10, "web", 1000)
	add("b", 1, 11, "db", 2000)
	add("c", 2, 10, "web-renamed", 3000)
	add("d", 2, 12, "gone", 4000)
	ids := func(f RecordingFilter) string {
		t.Helper()
		l, err := s.ListRecordings(ctx, f, 0, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, r := range l {
			out += r.ID
		}
		return out
	}
	for _, c := range []struct {
		f    RecordingFilter
		want string
	}{
		{RecordingFilter{}, "dcba"},
		{RecordingFilter{Owner: 1}, "ba"},
		{RecordingFilter{Host: 10}, "ca"},
		{RecordingFilter{Owner: 2, Host: 10}, "c"},
		{RecordingFilter{From: 2000, To: 4000}, "cb"}, // ([from, to))
		{RecordingFilter{Host: 99}, ""},
	} {
		if got := ids(c.f); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.f, got, c.want)
		}
	}
	// (The page after one, under a filter: the filter still holds.)
	l, err := s.ListRecordings(ctx, RecordingFilter{Host: 10}, 3000, "c", 100)
	if err != nil || len(l) != 1 || l[0].ID != "a" {
		t.Fatalf("next page: %+v %v", l, err)
	}
	hosts, owners, err := s.RecordingChoices(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(hosts) != "[{12 gone u2} {10 web-renamed u2} {11 db u1}]" || fmt.Sprint(owners) != "[{2 u2 } {1 u1 }]" {
		t.Fatalf("choices: %v %v", hosts, owners)
	}
	hosts, _, _ = s.RecordingChoices(ctx, 1)
	if fmt.Sprint(hosts) != "[{11 db u1} {10 web u1}]" {
		t.Fatalf("one owner's hosts: %v", hosts)
	}
}

// Audit entries narrowed by user (failed sign-ins as them, with no id,
// included), by kind of action (the first part; "other" all the rest) and
// by time.
func TestAuditFilters(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clock := int64(1000)
	s.now = func() time.Time { return time.Unix(clock, 0) }
	one := int64(1)
	two := int64(2)
	add := func(uid *int64, name, action string) {
		t.Helper()
		clock += 10
		target := ""
		if uid == nil {
			name, target = "", name // (a failed sign-in: no one's, the name tried its target — as auditAs writes it)
		}
		if err := s.Audit(ctx, uid, name, action, target, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	add(nil, " ALICE ", "login.failed") // 1010 (as typed: sign-in trims it, minds no case)
	add(&one, "alice", "login.success")
	add(&one, "alice", "convo.screen") // 1030
	add(&two, "bob", "host.create")
	add(&two, "bob", "brandnew.thing") // 1050
	add(&one, "alice", "logout")
	acts := func(q AuditQuery) string {
		t.Helper()
		l, err := s.ListAudit(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, e := range l {
			out += e.Action + " "
		}
		return out
	}
	for _, c := range []struct {
		name string
		q    AuditQuery
		want string
	}{
		{"user by id", AuditQuery{UserID: &one}, "logout convo.screen login.success "},
		{"user and their failed sign-ins", AuditQuery{UserID: &one, SignInAs: "alice"}, "logout convo.screen login.success login.failed "},
		{"not another's failed sign-ins", AuditQuery{UserID: &two, SignInAs: "bob"}, "brandnew.thing host.create "},
		{"kind", AuditQuery{Heads: []string{"login", "logout"}}, "logout login.success login.failed "},
		{"a head with no dot", AuditQuery{Heads: []string{"logout"}}, "logout "},
		{"other", AuditQuery{NotHeads: []string{"login", "logout", "convo", "host"}}, "brandnew.thing "},
		{"time", AuditQuery{From: 1030, To: 1050}, "host.create convo.screen "},
	} {
		if got := acts(c.q); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
