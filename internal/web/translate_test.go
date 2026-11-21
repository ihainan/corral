package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNeedsThoughtTranslation(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		target string
		want   bool
	}{
		{"english to zh-CN", "Let's inspect `internal/web/translate.go` and check the HTTP handlers.", "zh-CN", true},
		{"chinese to zh-CN", "我们先检查一下 `internal/web/translate.go` 里的路由配置。", "zh-CN", false},
		{"mixed mostly chinese to zh-CN", "这里调用 `apiConversation` 之后会触发 `agyMergeDBErrors` 合并数据库里的错误步骤。", "zh-CN", false},
		{"short text skipped", "ok done", "zh-CN", false},
		{"pure code block skipped", "```go\nfunc main() { fmt.Println(\"hello world\") }\n```", "zh-CN", false},
		{"chinese to en", "我们需要在后端加一个只读的数据库查询来补全错误步骤。", "en", true},
		{"english to en", "We should add a read-only database query to surface error steps.", "en", false},
		{"japanese to ja", "この関数はエラーメッセージを解析して表示します。", "ja", false},
		{"english to ja", "This function parses and renders error messages cleanly.", "ja", true},
		{"korean to ko", "이 함수는 오류 메시지를 파싱하고 표시합니다.", "ko", false},
		{"english to ko", "This function parses and renders error messages cleanly.", "ko", true},
	}
	for _, tc := range cases {
		if got := needsThoughtTranslation(tc.text, tc.target); got != tc.want {
			t.Errorf("%s: needsThoughtTranslation(%q, %q) = %v, want %v", tc.name, tc.text, tc.target, got, tc.want)
		}
	}
}

func TestTranslateConfigAndThoughtsAPI(t *testing.T) {
	h := newHarness(t)
	alice := h.admin()
	bob := h.member(alice, "bob")

	var calls atomic.Int32
	var sawReasoningReject atomic.Bool
	var authSeen string
	var mu sync.Mutex

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		authSeen = r.Header.Get("Authorization")
		mu.Unlock()
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		// Simulate an OpenAI-compatible proxy that rejects reasoning_effort on the first request.
		if _, has := req["reasoning_effort"]; has && sawReasoningReject.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field reasoning_effort"}}`))
			return
		}
		calls.Add(1)
		msgs, _ := req["messages"].([]any)
		userMsg := ""
		if len(msgs) >= 2 {
			if m, ok := msgs[1].(map[string]any); ok {
				userMsg, _ = m["content"].(string)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "译文：" + userMsg}},
			},
		})
	}))
	defer mockLLM.Close()

	// Inject mock HTTP client so httptest loopback URL works.
	h.srv.translateClient = mockLLM.Client()

	// 1. Default config: disabled, no key.
	r := alice.do("GET", "/api/me/translate", nil)
	mustStatus(t, r, 200, "get default")
	if r.json()["enabled"] != false || r.json()["has_key"] != false {
		t.Fatalf("unexpected default config: %s", r.Body)
	}

	// 2. Translating while disabled returns 409.
	r = alice.do("POST", "/api/translate/thoughts", map[string]any{
		"items": []map[string]string{{"text": "Let's check the logs for errors."}},
	})
	mustStatus(t, r, 409, "disabled translate")

	// 3. Save config with API key; secret must NEVER appear in response.
	secret := "sk-test-secret-never-leak-987654"
	r = alice.do("PUT", "/api/me/translate", map[string]any{
		"enabled":     true,
		"target_lang": "zh-CN",
		"base_url":    mockLLM.URL + "/v1",
		"model":       "deepseek-flash",
		"api_key":     secret,
	})
	mustStatus(t, r, 200, "save config")
	if strings.Contains(string(r.Body), secret) {
		t.Fatalf("API key leaked in PUT response: %s", r.Body)
	}
	if r.json()["has_key"] != true || r.json()["enabled"] != true {
		t.Fatalf("unexpected saved config: %s", r.Body)
	}

	// GET never returns plaintext key; Bob's config is isolated.
	r = alice.do("GET", "/api/me/translate", nil)
	mustStatus(t, r, 200, "get saved")
	if strings.Contains(string(r.Body), secret) {
		t.Fatalf("API key leaked in GET response: %s", r.Body)
	}
	rBob := bob.do("GET", "/api/me/translate", nil)
	mustStatus(t, rBob, 200, "bob isolated")
	if rBob.json()["enabled"] != false || rBob.json()["has_key"] != false {
		t.Fatalf("bob saw alice's translate config: %s", rBob.Body)
	}

	// 4. Test endpoint works and retries cleanly after reasoning_effort HTTP 400.
	r = alice.do("POST", "/api/me/translate/test", map[string]any{})
	mustStatus(t, r, 200, "test translate")
	if !sawReasoningReject.Load() {
		t.Fatalf("expected reasoning_effort 400 fallback path to execute")
	}
	mu.Lock()
	gotAuth := authSeen
	mu.Unlock()
	if gotAuth != "Bearer "+secret {
		t.Fatalf("Authorization = %q, want Bearer %s", gotAuth, secret)
	}

	// 5. Translate batch: 1 English (calls LLM), 1 Chinese (skipped locally, no LLM call).
	beforeCalls := calls.Load()
	engText := "Checking `convo_agy_db.go` to make sure quota errors render properly."
	zhText := "这里已经是中文思考过程，不需要再调用模型翻译。"
	r = alice.do("POST", "/api/translate/thoughts", map[string]any{
		"items": []map[string]string{
			{"text": engText},
			{"text": zhText},
		},
	})
	mustStatus(t, r, 200, "translate batch")
	if calls.Load() != beforeCalls+1 {
		t.Fatalf("expected exactly 1 LLM call (zh skipped), got %d", calls.Load()-beforeCalls)
	}
	var resp struct {
		TargetLang string `json:"target_lang"`
		Results    []struct {
			Hash   string `json:"hash"`
			Status string `json:"status"`
			Text   string `json:"text"`
			Cached bool   `json:"cached"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.Body), &resp); err != nil || len(resp.Results) != 2 {
		t.Fatalf("decode translate response: %v %s", err, r.Body)
	}
	if resp.Results[0].Status != "ok" || resp.Results[0].Cached || !strings.HasPrefix(resp.Results[0].Text, "译文：") {
		t.Fatalf("results[0] = %+v", resp.Results[0])
	}
	if resp.Results[1].Status != "skipped" || resp.Results[1].Text != "" {
		t.Fatalf("results[1] should be skipped: %+v", resp.Results[1])
	}

	// 6. Repeat request hits SQLite cache (zero new LLM calls).
	r2 := alice.do("POST", "/api/translate/thoughts", map[string]any{
		"items": []map[string]string{{"text": engText}},
	})
	mustStatus(t, r2, 200, "translate cached")
	if calls.Load() != beforeCalls+1 {
		t.Fatalf("expected cache hit with 0 new LLM calls, got %d", calls.Load()-beforeCalls)
	}
	if err := json.Unmarshal([]byte(r2.Body), &resp); err != nil || len(resp.Results) != 1 || !resp.Results[0].Cached || resp.Results[0].Status != "ok" {
		t.Fatalf("expected cached=true: %s", r2.Body)
	}

	// 7. Clear API key works.
	r = alice.do("PUT", "/api/me/translate", map[string]any{
		"enabled":   false,
		"clear_key": true,
	})
	mustStatus(t, r, 200, "clear key")
	if r.json()["has_key"] != false {
		t.Fatalf("expected has_key=false after clear: %s", r.Body)
	}
}
