package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/codexrpc"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

type nativeKey struct{ owner, host int64 }
type nativePeer struct {
	ready      chan struct{}
	client     *codexrpc.Client
	err        error
	mu         sync.Mutex
	controlled map[string]bool
}
type nativeCodex struct {
	mu     sync.Mutex
	peers  map[nativeKey]*nativePeer
	closed bool
}

func (p *nativePeer) controls(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.controlled[id]
}
func (p *nativePeer) control(id string) { p.mu.Lock(); p.controlled[id] = true; p.mu.Unlock() }

func (n *nativeCodex) close(owner int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if owner == 0 {
		n.closed = true
	}
	for k, p := range n.peers {
		if owner == 0 || k.owner == owner {
			delete(n.peers, k)
			select {
			case <-p.ready:
				if p.client != nil {
					p.client.Close()
				}
			default:
			}
		}
	}
}

func (s *Server) nativeConnection(ctx context.Context, sess *term.Session) (*nativePeer, error) {
	k := nativeKey{sess.OwnerID, sess.HostID}
	n := &s.native
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil, codexrpc.ErrClosed
	}
	if n.peers == nil {
		n.peers = map[nativeKey]*nativePeer{}
	}
	p := n.peers[k]
	if p != nil {
		select {
		case <-p.ready:
			if p.client != nil {
				select {
				case <-p.client.Done():
					delete(n.peers, k)
					p = nil
				default:
				}
			}
		default:
		}
	}
	fresh := p == nil
	if fresh {
		p = &nativePeer{ready: make(chan struct{}), controlled: map[string]bool{}}
		n.peers[k] = p
	}
	n.mu.Unlock()
	if fresh {
		// Setup is shared by the owner's tabs; cancelling one page must not
		// cancel another page's connection attempt.
		go func() {
			setup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var home string
			err := s.withSFTP(setup, sess, 10*time.Second, func(c *sftplite.Client) error { var e error; home, e = c.RealPath("."); return e })
			if err == nil {
				socket := path.Join(home, ".codex/app-server-control/app-server-control.sock")
				transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return sshx.DialVia(ctx, sess.Client(), "unix", socket, sshx.DialOptions{Aux: true})
				}, MaxResponseHeaderBytes: 32 << 10}
				p.client, err = codexrpc.Dial(setup, "ws://codex.local/", &http.Client{Transport: transport})
			}
			p.err = err
			n.mu.Lock()
			if n.closed || n.peers[k] != p {
				if p.client != nil {
					p.client.Close()
				}
			}
			if err != nil && n.peers[k] == p {
				delete(n.peers, k)
			}
			close(p.ready)
			n.mu.Unlock()
		}()
	}
	select {
	case <-p.ready:
		return p, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type nativeThread struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Preview  string `json:"preview"`
	Cwd      string `json:"cwd"`
	Model    string `json:"model"`
	Effort   string `json:"reasoningEffort"`
	CanInput *bool  `json:"canAcceptDirectInput,omitempty"`
	Status   struct {
		Type  string   `json:"type"`
		Flags []string `json:"activeFlags"`
	} `json:"status"`
}

func nativeError(w http.ResponseWriter, err error) {
	var rpc *codexrpc.RPCError
	var unknown *codexrpc.Unknown
	switch {
	case errors.As(err, &rpc):
		writeError(w, 422, "codex_rejected", "Codex did not accept this operation; refresh the conversation before trying again")
	case errors.As(err, &unknown):
		writeError(w, 409, "outcome_unknown", "the operation may have been accepted; refresh the conversation before sending again")
	case errors.Is(err, codexrpc.ErrStale):
		writeError(w, 409, "stale_request", "this request has already been answered or withdrawn")
	default:
		writeError(w, 502, "codex_unavailable", "cannot connect to Codex on this host; its app server must already be running")
	}
}

func (s *Server) nativeRequest(w http.ResponseWriter, r *http.Request) (*term.Session, *nativePeer, context.Context, context.CancelFunc) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return nil, nil, nil, func() {}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	p, err := s.nativeConnection(ctx, sess)
	if err != nil {
		cancel()
		nativeError(w, err)
		return nil, nil, nil, func() {}
	}
	w.Header().Set("Cache-Control", "no-store")
	return sess, p, ctx, cancel
}

func nativeID(w http.ResponseWriter, r *http.Request) string {
	id := r.PathValue("thread")
	if !convoUUID.MatchString(id) {
		writeError(w, 400, "bad_thread", "invalid conversation id")
		return ""
	}
	return id
}

func readNativeThread(ctx context.Context, p *nativePeer, id string) (nativeThread, error) {
	var res struct {
		Thread nativeThread `json:"thread"`
	}
	err := p.client.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &res)
	if err == nil && res.Thread.ID != id {
		err = errors.New("Codex returned a different thread")
	}
	return res.Thread, err
}

func (s *Server) apiCodexThreads(w http.ResponseWriter, r *http.Request) {
	_, p, ctx, cancel := s.nativeRequest(w, r)
	defer cancel()
	if p == nil {
		return
	}
	q := r.URL.Query()
	cursor := q.Get("cursor")
	if len(cursor) > 4096 {
		writeError(w, 400, "bad_cursor", "invalid cursor")
		return
	}
	params := map[string]any{"limit": 30, "sortKey": "updated_at", "sortDirection": "desc", "sourceKinds": []string{"cli", "vscode", "appServer"}, "modelProviders": []string{}}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var res struct {
		Data []nativeThread `json:"data"`
		Next *string        `json:"nextCursor"`
	}
	if err := p.client.Call(ctx, "thread/list", params, &res); err != nil {
		nativeError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) apiCodexThread(w http.ResponseWriter, r *http.Request) {
	id := nativeID(w, r)
	if id == "" {
		return
	}
	_, p, ctx, cancel := s.nativeRequest(w, r)
	defer cancel()
	if p == nil {
		return
	}
	thread, err := readNativeThread(ctx, p, id)
	if err != nil {
		nativeError(w, err)
		return
	}
	params := map[string]any{"threadId": id, "limit": 40, "sortDirection": "desc"}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		if len(cursor) > 4096 {
			writeError(w, 400, "bad_cursor", "invalid cursor")
			return
		}
		params["cursor"] = cursor
	}
	var page json.RawMessage
	seq, err := p.client.CallAt(ctx, "thread/items/list", params, &page)
	if err != nil {
		var rpc *codexrpc.RPCError
		// Codex materializes history on the first turn. Before then its
		// history backend can report -32601 even for a loaded empty thread.
		if p.controls(id) && thread.Preview == "" && thread.Status.Type == "idle" && errors.As(err, &rpc) && rpc.Code == -32601 {
			page = json.RawMessage(`{"data":[],"nextCursor":null}`)
			_, seq, _, _ = p.client.Events(0, id)
		} else {
			nativeError(w, err)
			return
		}
	}
	var turns struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if thread.Status.Type == "active" {
		if err = p.client.Call(ctx, "thread/turns/list", map[string]any{"threadId": id, "limit": 1, "itemsView": "notLoaded", "sortDirection": "desc"}, &turns); err != nil {
			nativeError(w, err)
			return
		}
	}
	active := ""
	for _, t := range turns.Data {
		if t.Status == "inProgress" {
			active = t.ID
		}
	}
	writeJSONCompressed(w, r, 200, map[string]any{"thread": thread, "page": page, "seq": seq, "connected": p.controls(id), "turn": active, "requests": p.client.Requests(id)})
}

func (s *Server) apiCodexModels(w http.ResponseWriter, r *http.Request) {
	_, p, ctx, cancel := s.nativeRequest(w, r)
	defer cancel()
	if p == nil {
		return
	}
	var res json.RawMessage
	if err := p.client.Call(ctx, "model/list", map[string]any{"includeHidden": false}, &res); err != nil {
		nativeError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

type nativeCommand struct {
	Action      string              `json:"action"`
	Key         string              `json:"key"`
	Text        string              `json:"text"`
	Images      []string            `json:"images"`
	Turn        string              `json:"turn"`
	Cwd         string              `json:"cwd"`
	Model       string              `json:"model"`
	Effort      string              `json:"effort"`
	ResetEffort bool                `json:"reset_effort"`
	Request     json.RawMessage     `json:"request"`
	Decision    string              `json:"decision"`
	Answers     map[string][]string `json:"answers"`
}

func (s *Server) apiCodexCommand(w http.ResponseWriter, r *http.Request) {
	var b nativeCommand
	if err := readJSON(r, &b); err != nil || !newAgentKey.MatchString(b.Key) {
		writeError(w, 400, "bad_request", "a command key is required")
		return
	}
	id := r.PathValue("thread")
	if id != "" && !convoUUID.MatchString(id) {
		writeError(w, 400, "bad_thread", "invalid conversation id")
		return
	}
	if (b.Action == "create") != (id == "") {
		writeError(w, 400, "bad_action", "invalid conversation operation")
		return
	}
	switch b.Action {
	case "create", "connect", "send", "interrupt", "settings", "reply":
	default:
		writeError(w, 400, "bad_action", "unsupported operation")
		return
	}
	if len(b.Text) > 100<<10 || len(b.Model) > 128 || len(b.Cwd) > 4096 || len(b.Turn) > 128 || len(b.Request) > 128 {
		writeError(w, 400, "bad_request", "input is too long")
		return
	}
	if len(b.Images) > 8 {
		writeError(w, 400, "bad_images", "too many images")
		return
	}
	for _, image := range b.Images {
		if !path.IsAbs(image) || len(image) > 4096 || strings.ContainsAny(image, "\x00\r\n") {
			writeError(w, 400, "bad_images", "invalid image path")
			return
		}
	}
	if b.Action == "send" && strings.TrimSpace(b.Text) == "" && len(b.Images) == 0 {
		writeError(w, 400, "bad_text", "a message is required")
		return
	}
	if b.Action == "create" && (!path.IsAbs(b.Cwd) || strings.ContainsAny(b.Cwd, "\x00\r\n")) {
		writeError(w, 400, "bad_folder", "an absolute project folder is required")
		return
	}
	if b.Action == "interrupt" && b.Turn == "" {
		writeError(w, 400, "bad_turn", "an active turn is required")
		return
	}
	if b.Effort != "" && b.Effort != "none" && b.Effort != "minimal" && b.Effort != "low" && b.Effort != "medium" && b.Effort != "high" && b.Effort != "xhigh" && b.Effort != "max" {
		writeError(w, 400, "bad_effort", "invalid reasoning effort")
		return
	}
	sess, p, ctx, cancel := s.nativeRequest(w, r)
	defer cancel()
	if p == nil {
		return
	}
	if id != "" {
		thread, err := readNativeThread(ctx, p, id)
		if err != nil {
			nativeError(w, err)
			return
		}
		if b.Action != "connect" && !p.controls(id) {
			writeError(w, 409, "not_connected", "connect this conversation before controlling it")
			return
		}
		if thread.CanInput != nil && !*thread.CanInput {
			writeError(w, 409, "read_only", "this conversation does not accept direct input")
			return
		}
	}
	encoded, _ := json.Marshal(struct {
		Thread string
		Body   nativeCommand
	}{id, b})
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	previous, fresh, err := s.store.BeginCodexCommand(ctx, sess.OwnerID, sess.HostID, b.Key, digest)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !fresh {
		if previous.Digest != digest {
			writeError(w, 409, "key_reused", "this key belongs to different input")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(previous.Status)
		w.Write([]byte(previous.Result))
		return
	}
	params := map[string]any{"threadId": id}
	method := ""
	var res json.RawMessage
	switch b.Action {
	case "create":
		method = "thread/start"
		params = map[string]any{"cwd": b.Cwd}
		if b.Model != "" {
			params["model"] = b.Model
		}
	case "connect":
		method = "thread/resume"
		params["excludeTurns"] = true
	case "send":
		input := []map[string]string{}
		if b.Text != "" {
			input = append(input, map[string]string{"type": "text", "text": b.Text})
		}
		for _, image := range b.Images {
			input = append(input, map[string]string{"type": "localImage", "path": image})
		}
		params["input"] = input
		params["clientUserMessageId"] = b.Key
		if b.Turn != "" {
			method = "turn/steer"
			params["expectedTurnId"] = b.Turn
		} else {
			method = "turn/start"
		}
	case "interrupt":
		method = "turn/interrupt"
		params["turnId"] = b.Turn
	case "settings":
		method = "thread/settings/update"
		if b.Model != "" {
			params["model"] = b.Model
		}
		if b.Effort != "" {
			params["effort"] = b.Effort
		} else if b.ResetEffort {
			params["effort"] = nil
		}
	case "reply":
		err = p.client.Reply(ctx, id, b.Request, func(e codexrpc.Event) (any, error) { return nativeDecision(e, b) })
	}
	if err == nil && method != "" {
		err = p.client.Call(ctx, method, params, &res)
	}
	status := 200
	body := map[string]any{"accepted": true}
	if err == nil {
		if b.Action == "create" || b.Action == "connect" {
			var result struct {
				Thread nativeThread `json:"thread"`
			}
			if json.Unmarshal(res, &result) != nil || !convoUUID.MatchString(result.Thread.ID) || (id != "" && result.Thread.ID != id) {
				err = &codexrpc.Unknown{Cause: errors.New("invalid thread response")}
			} else {
				p.control(result.Thread.ID)
				body["thread"] = result.Thread
			}
		}
		if b.Action == "send" {
			var result struct {
				Turn json.RawMessage `json:"turn"`
			}
			_ = json.Unmarshal(res, &result)
			body["turn"] = result.Turn
		}
	}
	if err != nil {
		status = 409
		body = map[string]any{"error": "outcome_unknown", "message": "refresh the conversation to check whether this operation was accepted"}
		var rpc *codexrpc.RPCError
		var unknown *codexrpc.Unknown
		if errors.As(err, &rpc) || errors.Is(err, codexrpc.ErrStale) || !errors.As(err, &unknown) {
			status = 422
			body = map[string]any{"error": "codex_rejected", "message": "Codex did not accept this operation"}
		}
	}
	out, _ := json.Marshal(body)
	settle, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer stop()
	if e := s.store.FinishCodexCommand(settle, sess.OwnerID, sess.HostID, b.Key, status, string(out)); e != nil {
		writeError(w, 409, "outcome_unknown", "the result could not be saved; refresh the conversation")
		return
	}
	s.audit(r.WithContext(settle), "codex."+b.Action, sess.HostName, map[string]any{"thread": id, "outcome": status})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(out)
}

func nativeDecision(e codexrpc.Event, b nativeCommand) (any, error) {
	var p struct {
		Available   []json.RawMessage `json:"availableDecisions"`
		Permissions json.RawMessage   `json:"permissions"`
		Questions   []struct {
			ID string `json:"id"`
		} `json:"questions"`
	}
	if json.Unmarshal(e.Params, &p) != nil {
		return nil, codexrpc.ErrStale
	}
	switch e.Method {
	case "item/tool/requestUserInput":
		if len(p.Questions) > 20 {
			return nil, codexrpc.ErrStale
		}
		out := map[string]any{}
		for _, q := range p.Questions {
			answers := b.Answers[q.ID]
			if len(answers) == 0 || len(answers) > 20 {
				return nil, errors.New("missing answer")
			}
			for _, a := range answers {
				if len(a) > 16000 {
					return nil, errors.New("answer too long")
				}
			}
			out[q.ID] = map[string]any{"answers": answers}
		}
		return map[string]any{"answers": out}, nil
	case "item/permissions/requestApproval":
		if b.Decision == "decline" {
			return map[string]any{"permissions": map[string]any{}}, nil
		}
		if b.Decision != "accept" || len(p.Permissions) == 0 {
			return nil, errors.New("invalid permission decision")
		}
		return map[string]any{"permissions": p.Permissions, "scope": "turn"}, nil
	case "mcpServer/elicitation/request":
		if b.Decision != "decline" && b.Decision != "cancel" {
			return nil, errors.New("elicitation acceptance is not supported")
		}
		return map[string]any{"action": b.Decision, "content": nil}, nil
	default:
		if b.Decision != "accept" && b.Decision != "decline" && b.Decision != "cancel" {
			return nil, errors.New("invalid approval decision")
		}
		if len(p.Available) > 0 {
			found := false
			for _, option := range p.Available {
				var value string
				if json.Unmarshal(option, &value) == nil && value == b.Decision {
					found = true
				}
			}
			if !found {
				return nil, errors.New("decision is not offered")
			}
		}
		return map[string]any{"decision": b.Decision}, nil
	}
}

func (s *Server) apiCodexEvents(w http.ResponseWriter, r *http.Request) {
	id := nativeID(w, r)
	if id == "" {
		return
	}
	sess, p, ctx, cancel := s.nativeRequest(w, r)
	defer cancel()
	if p == nil {
		return
	}
	if _, err := readNativeThread(ctx, p, id); err != nil {
		nativeError(w, err)
		return
	}
	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		writeError(w, 401, "unauthorized", "please sign in")
		return
	}
	life, stop := context.WithDeadline(r.Context(), expiry)
	defer stop()
	untrack, ok := s.trackSocket(currentUser(r).ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: stop})
	if !ok {
		writeError(w, 429, "too_many_windows", "too many event streams")
		return
	}
	defer untrack()
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		after, _ = strconv.ParseUint(last, 10, 64)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	send := func(seq uint64, data any) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		b, _ := json.Marshal(data)
		_, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, b)
		if err == nil {
			err = controller.Flush()
		}
		return err == nil
	}
	if !send(after, map[string]any{"method": "ready", "seq": after, "requests": p.client.Requests(id)}) {
		return
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		events, seq, gap, changed := p.client.Events(after, id)
		if gap {
			if !send(seq, map[string]any{"method": "resync", "seq": seq}) {
				return
			}
			after = seq
		} else {
			for _, e := range events {
				if !send(e.Seq, e) {
					return
				}
			}
			after = seq
		}
		select {
		case <-life.Done():
			return
		case <-sess.Finished():
			return
		case <-p.client.Done():
			return
		case <-changed:
		case <-tick.C:
			if _, err := s.auth.CheckLogin(life, loginHash(r)); err != nil {
				return
			}
			if !send(after, map[string]any{"method": "heartbeat", "seq": after}) {
				return
			}
		}
	}
}
