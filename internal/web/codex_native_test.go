package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ihainan/corral/internal/codexrpc"
)

const nativeTestThread = "01a1cd1d-b2d0-7341-95d1-b5323eefbb97"

func codexEvent(method, params string) codexrpc.Event {
	return codexrpc.Event{Method: method, Params: json.RawMessage(params)}
}

// Read-only opt-in against an existing daemon. The Corral account, HTTP
// login and SSH server are all isolated fixtures, never deployment accounts.
func TestNativeCodexLiveReadOnly(t *testing.T) {
	home:=os.Getenv("CORRAL_CODEX_READONLY_HOME")
	if home==""{t.Skip("set CORRAL_CODEX_READONLY_HOME for existing daemon read-only verification")}
	h:=newHarness(t);c:=h.admin();ssh:=h.sshServer(home);host:=h.hostFor(c,ssh.Addr.Port)
	opened:=c.do("POST","/api/sessions",map[string]any{"host_id":host});mustStatus(t,opened,201,"isolated transport")
	base:="/api/sessions/"+opened.json()["id"].(string)+"/codex"
	start:=time.Now();listed:=c.do("GET",base+"/threads",nil);mustStatus(t,listed,200,"actual daemon list")
	var threads struct{Data []nativeThread `json:"data"`};if err:=json.Unmarshal(listed.Body,&threads);err!=nil||len(threads.Data)==0{t.Fatalf("thread list: %v",err)}
	var durations []time.Duration
	for _,thread:=range threads.Data[:min(3,len(threads.Data))]{
		began:=time.Now();got:=c.do("GET",base+"/threads/"+thread.ID,nil);mustStatus(t,got,200,"actual daemon paginated snapshot");durations=append(durations,time.Since(began))
		if got.json()["connected"]!=false {t.Fatal("read-only inspection claimed control")}
		page:=got.json()["page"].(map[string]any);if len(page["data"].([]any))>40{t.Fatal("history page exceeded requested bound")}
	}
	mustStatus(t,c.do("GET",base+"/models",nil),200,"actual daemon model list")
	if len(ssh.Commands())!=0{t.Fatalf("native reads executed %d shell commands",len(ssh.Commands()))}
	t.Logf("actual daemon over HTTP + isolated SSH: %d threads, snapshot times %v, total %s; zero shell commands or control operations",len(threads.Data),durations,time.Since(start))
}

func TestNativeCodexHTTPAndSSH(t *testing.T) {
	h := newHarness(t)
	alice := h.admin()
	bob := h.member(alice, "bob")
	root := t.TempDir()
	socket := filepath.Join(root, ".codex/app-server-control/app-server-control.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	seen := map[string]map[string]any{}
	engine := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params map[string]any  `json:"params"`
			}
			if json.Unmarshal(raw, &m) != nil {
				return
			}
			if len(m.ID) == 0 {
				continue
			}
			mu.Lock()
			counts[m.Method]++
			seen[m.Method] = m.Params
			mu.Unlock()
			thread := map[string]any{"id": nativeTestThread, "cwd": root, "preview": "fixture", "model": "test-model", "reasoningEffort": "high", "canAcceptDirectInput": true, "status": map[string]string{"type": "idle"}}
			result := any(map[string]any{})
			switch m.Method {
			case "thread/list":
				result = map[string]any{"data": []any{thread}, "nextCursor": nil}
			case "thread/read", "thread/start", "thread/resume":
				result = map[string]any{"thread": thread}
			case "thread/items/list":
				result = map[string]any{"data": []any{map[string]any{"turnId": "turn", "item": map[string]string{"id": "answer", "type": "agentMessage", "text": "**ready**"}, "completedAtMs": 1}}, "nextCursor": nil}
			case "turn/start":
				if m.Params["clientUserMessageId"] == "lost-response" {
					return
				}
				result = map[string]any{"turn": map[string]string{"id": "turn"}}
			}
			out, _ := json.Marshal(map[string]any{"id": m.ID, "result": result})
			if conn.Write(r.Context(), websocket.MessageText, out) != nil {
				return
			}
		}
	})}
	go engine.Serve(listener)
	t.Cleanup(func() { engine.Close() })
	ssh := h.sshServer(root)
	host := h.hostFor(alice, ssh.Addr.Port)
	opened := alice.do("POST", "/api/sessions", map[string]any{"host_id": host})
	mustStatus(t, opened, 201, "open isolated SSH connection")
	session := opened.json()["id"].(string)
	base := "/api/sessions/" + session + "/codex"
	at := base + "/threads/" + nativeTestThread
	mustStatus(t, bob.do("GET", base+"/threads", nil), 404, "cross-owner list")
	mustStatus(t, alice.do("GET", base+"/threads", nil), 200, "native list over SSH Unix socket")
	mustStatus(t, alice.do("GET", at, nil), 200, "read-only snapshot")
	send := map[string]any{"action": "send", "key": "message-key", "text": "hello"}
	mustStatus(t, alice.do("POST", at, send), 409, "writing before explicit connect")
	mustStatus(t, alice.do("POST", at, map[string]any{"action": "connect", "key": "connect-key"}), 200, "explicit connect")
	mustStatus(t, alice.do("POST", at, send), 200, "send")
	mustStatus(t, alice.do("POST", at, send), 200, "duplicate send returns saved result")
	mustStatus(t, alice.do("POST", at, map[string]any{"action": "send", "key": "message-key", "text": "different"}), 409, "key cannot be reused for different text")
	mustStatus(t, alice.do("POST", at, map[string]any{"action": "settings", "key": "settings-key", "model": "another", "effort": "low"}), 200, "thread settings")
	mustStatus(t, alice.do("POST", at, map[string]any{"action": "settings", "key": "reset-effort", "reset_effort": true}), 200, "reset effort")
	mustStatus(t, bob.do("POST", at, send), 404, "cross-owner send")
	mustStatus(t, alice.do("POST", base+"/threads", map[string]any{"action": "create", "key": "invalid-folder", "cwd": "relative"}), 400, "invalid folder is rejected locally")
	mu.Lock()
	if counts["turn/start"] != 1 {
		t.Errorf("message sent %d times", counts["turn/start"])
	}
	for _, method := range []string{"thread/resume", "turn/start", "thread/settings/update"} {
		for _, forbidden := range []string{"approvalPolicy", "sandbox", "permissions", "config", "approvalsReviewer"} {
			if _, ok := seen[method][forbidden]; ok {
				t.Errorf("%s overwrote %s", method, forbidden)
			}
		}
	}
	if _, ok := seen["thread/settings/update"]["effort"]; !ok || seen["thread/settings/update"]["effort"] != nil {
		t.Error("default effort was not reset")
	}
	mu.Unlock()
	// An interrupted transport must leave a durable unknown outcome, and a
	// repeated HTTP request must not submit the message again after reconnect.
	unknown := map[string]any{"action": "send", "key": "lost-response", "text": "possibly accepted"}
	mustStatus(t, alice.do("POST", at, unknown), 409, "lost acceptance stays unknown")
	mustStatus(t, alice.do("POST", at, map[string]any{"action": "connect", "key": "reconnect-key"}), 200, "reconnect after transport loss")
	mustStatus(t, alice.do("POST", at, unknown), 409, "unknown is never retried")
	mu.Lock()
	if counts["turn/start"] != 2 {
		t.Errorf("unknown message resubmitted: %d starts", counts["turn/start"])
	}
	mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.ts.URL+at+"/events", nil)
	req.AddCookie(&http.Cookie{Name: h.srv.cookieName(), Value: alice.cookie})
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "id:") {
		t.Fatalf("SSE: %q %v", line, err)
	}
	mustStatus(t, alice.do("POST", "/api/logout", map[string]any{}), 200, "logout")
	_, err = io.ReadAll(reader)
	if ctx.Err() != nil {
		t.Fatal("SSE survived login revocation")
	}
	if err != nil && !strings.Contains(err.Error(), "closed") {
		t.Logf("stream ended on revocation: %v", err)
	}
}

func TestNativeApprovalDecisionScope(t *testing.T) {
	for _, decision := range []string{"acceptForSession", "acceptWithExecpolicyAmendment", "invalid"} {
		_, err := nativeDecision(codexEvent("item/commandExecution/requestApproval", `{"availableDecisions":["accept","decline"]}`), nativeCommand{Decision: decision})
		if err == nil {
			t.Fatalf("persistent or unknown decision %q accepted", decision)
		}
	}
	if _, err := nativeDecision(codexEvent("item/commandExecution/requestApproval", `{"availableDecisions":["decline"]}`), nativeCommand{Decision: "accept"}); err == nil {
		t.Fatal("unoffered decision accepted")
	}
	result, err := nativeDecision(codexEvent("item/permissions/requestApproval", `{"permissions":{"network":{"enabled":true}}}`), nativeCommand{Decision: "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(result.(map[string]any)["scope"]) != "turn" {
		t.Fatal("permission was not limited to this turn")
	}
}
