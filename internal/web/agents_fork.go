package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Forking: a copy of an agent's conversation taken up in a workspace of its
// own (the original goes on as it was; the project's folder is the same
// one, not copied). Codex's by its own way (`fork <id>`). Claude Code's:
// its log copied as a session of its own and taken up (--resume <copy>):
// all of it, or of it up to the end of one turn (no agent has a way for
// that), up to the next message the person typed — what the log was then,
// a state it was in. (Not --fork-session for all of it: its copy is not
// written till the first message to it, and the page would show nothing
// of it till then.)

var (
	errForkNotFound = errors.New("no such session")
	errForkAt       = errors.New("not a message the person typed")
)

const (
	maxForkCopy = 512 << 20 // (a log's start copied, at most)
	maxForkLine = 1 << 20   // (the line cut before, read to check it)
)

// forkSource finds the log of an agent session on the host, by its id: its
// path and the folder it began in. Not followed: links (find does not).
func (s *Server) forkSource(ctx context.Context, sess *term.Session, kind, id string) (src, cwd string, err error) {
	if !sessionUUID.MatchString(id) {
		return "", "", errForkNotFound
	}
	if kind != "claude" && kind != "codex" {
		return "", "", errForkNotFound
	}
	out, err := sshx.Output(ctx, sess.Client(), forkFindCommand(kind, id), 8<<10)
	if err != nil {
		return "", "", err
	}
	found := strings.Fields(strings.TrimSpace(string(out)))
	if len(found) != 1 || strings.ContainsAny(string(out), "\r") {
		return "", "", errForkNotFound // (none, or two alike: not guessed between)
	}
	src = strings.TrimSpace(string(out))
	err = s.withSFTP(ctx, sess, convoReadLimit, func(c *sftplite.Client) error {
		// (Codex's first line has its instructions in it: tens of KB.)
		n := sessionHead
		if kind == "codex" {
			n = maxForkLine
		}
		head, err := c.ReadRange(src, 0, n)
		if err != nil {
			return err
		}
		if kind == "claude" {
			_, cwd = readSessionHead(head)
			return nil
		}
		line, _, _ := bytes.Cut(head, []byte("\n"))
		var m struct {
			Type    string `json:"type"`
			Payload struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &m) == nil && m.Type == "session_meta" && m.Payload.ID == id {
			cwd = m.Payload.Cwd
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(cwd, "/") {
		return "", "", errForkNotFound // (no folder told: not a log of that kind)
	}
	return src, cwd, nil
}

// forkCopy writes the start of Claude Code's log src, up to the line of the
// message at (one the person typed), as a session of its own next to it,
// its id made here: the new id. The line is found on the host (the first
// with that uuid: it is where it was made; a later one names it only as a
// parent, "parentUuid"), read here to check it is such a message.
func (s *Server) forkCopy(ctx context.Context, sess *term.Session, src, from, at string) (string, error) {
	if (at != "" && !sessionUUID.MatchString(at)) || !sessionUUID.MatchString(from) {
		return "", errForkAt
	}
	out, err := sshx.Output(ctx, sess.Client(), forkCutCommand(src, at), 256)
	if err != nil {
		return "", errForkAt
	}
	off, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || off <= 0 {
		return "", errForkAt
	}
	if off > maxForkCopy {
		return "", fmt.Errorf("the conversation up to there is too long to copy (%d MB at most)", maxForkCopy>>20)
	}
	if at != "" {
		err = s.forkCheckAt(ctx, sess, src, off, at)
		if err != nil {
			return "", err
		}
	}
	id := newSessionID()
	dst := src[:strings.LastIndexByte(src, '/')+1] + id + ".jsonl"
	out, err = sshx.Output(ctx, sess.Client(), forkCopyCommand(src, dst, off, from, id), 64)
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		return "", errors.New("the conversation could not be copied")
	}
	return id, nil
}

// forkCheckAt: the line at off of log src is the message at, one the
// person typed.
func (s *Server) forkCheckAt(ctx context.Context, sess *term.Session, src string, off int64, at string) error {
	return s.withSFTP(ctx, sess, convoReadLimit, func(c *sftplite.Client) error {
		b, err := c.ReadRange(src, off, maxForkLine)
		if err != nil {
			return err
		}
		line, _, ok := bytes.Cut(b, []byte("\n"))
		if !ok && len(b) >= maxForkLine {
			return errForkAt // (too long to check)
		}
		if !typedMessage(line, at) {
			return errForkAt
		}
		return nil
	})
}

// forkFindCommand: the log of Claude Code's or Codex's session id (a UUID)
// in the host account's own folders, links not followed: its path, for
// each found (at most two).
func forkFindCommand(kind, id string) string {
	if kind == "codex" {
		return `find "${CODEX_HOME:-$HOME/.codex}/sessions" -mindepth 4 -maxdepth 4 -type f -name ` + sshx.QuoteShell("rollout-*-"+id+".jsonl") + ` 2>/dev/null | head -n 2`
	}
	return `find "$HOME/.claude/projects" -mindepth 2 -maxdepth 2 -type f -name ` + sshx.QuoteShell(id+".jsonl") + ` 2>/dev/null | head -n 2`
}

// forkCutCommand: how many bytes of log src come before the first line
// with uuid at (a UUID); nothing (and failing) where none has it. No at:
// all its whole lines (one being written as it is read, not).
func forkCutCommand(src, at string) string {
	q := sshx.QuoteShell
	if at == "" {
		return `f=` + q(src) + `; n=$(wc -l < "$f") && head -n "$n" "$f" | wc -c`
	}
	return `f=` + q(src) + `; n=$(grep -n -m 1 -F ` + q(`"uuid":"`+at+`"`) + ` "$f" | cut -d: -f1); [ -n "$n" ] && head -n $((n-1)) "$f" | wc -c`
}

// forkCopyCommand writes the first off bytes of src to dst, the session id
// from (a UUID) made id (one) where the lines say it: only the account
// reads it (as Claude Code's own), nothing put where one is, nothing left
// half written. "ok" once it is there.
func forkCopyCommand(src, dst string, off int64, from, id string) string {
	q := sshx.QuoteShell
	return `umask 077; d=` + q(dst) + `; [ -e "$d" ] && exit 3; head -c ` + strconv.FormatInt(off, 10) + ` ` + q(src) +
		` | sed ` + q(`s/"sessionId":"`+from+`"/"sessionId":"`+id+`"/g`) + ` > "$d.tmp" && mv "$d.tmp" "$d" && echo ok || { rm -f "$d.tmp"; exit 1; }`
}

// dropForkCopy removes a copy forkCopy made, for an agent that did not
// start on it.
func (s *Server) dropForkCopy(sess *term.Session, src, id string) {
	if !sessionUUID.MatchString(id) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = sshx.Output(ctx, sess.Client(), forkDropCommand(src[:strings.LastIndexByte(src, '/')+1]+id+".jsonl"), 64)
}

// forkDropCommand removes a copy forkCopyCommand made.
func forkDropCommand(copy string) string {
	return `rm -f ` + sshx.QuoteShell(copy)
}

// typedMessage: line is Claude Code's record of a message the person typed,
// of uuid at (not a tool's result, not one of its own notes).
func typedMessage(line []byte, at string) bool {
	var r struct {
		Type        string `json:"type"`
		UUID        string `json:"uuid"`
		IsMeta      bool   `json:"isMeta"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "user" || r.UUID != at || r.IsMeta || r.IsSidechain || r.Message.Role != "user" {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(r.Message.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return false
			}
		}
	}
	return true
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
