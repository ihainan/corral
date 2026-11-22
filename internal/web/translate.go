package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/vault"
)

const (
	translateMaxItems       = 16
	translateMaxTextBytes   = 32 << 10
	translateMaxRespBytes   = 256 << 10
	translateMaxBaseURLLen  = 512
	translateMaxModelLen    = 128
	translateMaxKeyLen      = 4096
	translateMaxConcurrency = 4
	translateUpstreamTime   = 30 * time.Second
)

var translateLangNames = map[string]string{
	"zh-CN": "Simplified Chinese",
	"zh-TW": "Traditional Chinese",
	"en":    "English",
	"ja":    "Japanese",
	"ko":    "Korean",
	"fr":    "French",
	"de":    "German",
	"es":    "Spanish",
}

func translateAAD(userID int64) []byte {
	return []byte(fmt.Sprintf("translate:%d", userID))
}

func translateHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func normalizeTranslateBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > translateMaxBaseURLLen {
		return "", errors.New("base URL is required and must be at most 512 characters")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("base URL must be a valid http or https URL")
	}
	if u.User != nil {
		return "", errors.New("base URL must not include embedded credentials")
	}
	return strings.TrimRight(s, "/"), nil
}

func translateEndpointURL(baseURL string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(b, "/chat/completions") {
		return b
	}
	return b + "/chat/completions"
}

// blockedTranslatePrefixes: link-local / cloud metadata / unspecified addresses
// that outbound translation requests must never dial.
var blockedTranslatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("fe80::/10"),
}

func translateAddrBlocked(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() {
		return true
	}
	for _, p := range blockedTranslatePrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func newTranslateHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if ip, err := netip.ParseAddr(host); err == nil {
				if translateAddrBlocked(ip) {
					return nil, errors.New("destination address is blocked")
				}
				return dialer.DialContext(ctx, network, addr)
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if translateAddrBlocked(ip) {
					return nil, errors.New("destination resolves to a blocked address")
				}
			}
			var lastErr error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("no addresses resolved")
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Timeout:   translateUpstreamTime,
		Transport: tr,
		// Refuse redirects so Bearer API keys never leak to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not allowed for translation requests")
		},
	}
}

var (
	reFencedCode = regexp.MustCompile("(?s)```.*?```")
	reInlineCode = regexp.MustCompile("`[^`]*`")
	reMdLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	reURL        = regexp.MustCompile(`https?://\S+`)
	reUnixPath   = regexp.MustCompile(`(?:~|\.{1,2})?/(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]*`)
)

// needsThoughtTranslation performs fast local script detection after stripping
// code blocks, inline code, URLs, and file paths. Returns false when text is
// too short or already predominantly in targetLang, saving unnecessary API calls.
func needsThoughtTranslation(text, targetLang string) bool {
	if _, ok := translateLangNames[targetLang]; !ok {
		return false
	}
	clean := reFencedCode.ReplaceAllString(text, " ")
	clean = reInlineCode.ReplaceAllString(clean, " ")
	clean = reMdLink.ReplaceAllString(clean, "$1")
	clean = reURL.ReplaceAllString(clean, " ")
	clean = reUnixPath.ReplaceAllString(clean, " ")

	var han, kana, hangul, latin, total int
	for _, r := range clean {
		if !unicode.IsLetter(r) {
			continue
		}
		total++
		switch {
		case unicode.Is(unicode.Han, r):
			han++
		case unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			kana++
		case unicode.Is(unicode.Hangul, r):
			hangul++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}
	if total < 8 {
		return false
	}
	switch targetLang {
	case "zh-CN", "zh-TW":
		return float64(han)/float64(total) < 0.30
	case "en":
		if han == 0 && kana == 0 && hangul == 0 && float64(latin)/float64(total) >= 0.75 {
			return false
		}
		return true
	case "ja":
		if kana > 0 && float64(kana+han)/float64(total) >= 0.30 {
			return false
		}
		return true
	case "ko":
		return float64(hangul)/float64(total) < 0.30
	default:
		// Latin-script European targets (fr/de/es): let the LLM decide & preserve if already in target.
		return true
	}
}

type translateFlightState struct {
	mu    sync.Mutex
	calls map[string]*translateCall
	sems  map[int64]chan struct{}
}

type translateCall struct {
	done chan struct{}
	text string
	err  error
}

func (f *translateFlightState) sem(userID int64) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sems == nil {
		f.sems = map[int64]chan struct{}{}
	}
	ch := f.sems[userID]
	if ch == nil {
		ch = make(chan struct{}, translateMaxConcurrency)
		f.sems[userID] = ch
	}
	return ch
}

func (f *translateFlightState) do(key string, fn func() (string, error)) (string, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*translateCall{}
	}
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		<-c.done
		return c.text, c.err
	}
	c := &translateCall{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()

	c.text, c.err = fn()
	close(c.done)

	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	return c.text, c.err
}

func normalizeTranslateMode(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case "", "translate":
		return "translate", true
	case "summarize":
		return "summarize", true
	default:
		return "", false
	}
}

func translateCacheLang(mode, targetLang string) string {
	if mode == "summarize" {
		return "sum:" + targetLang
	}
	return targetLang
}

func needsThoughtProcessing(text, targetLang, mode string) bool {
	if needsThoughtTranslation(text, targetLang) {
		return true
	}
	// In summarize mode, also summarize long reasoning traces that are already
	// in the target language so verbose multi-paragraph thinking gets a crisp TL;DR.
	if mode == "summarize" {
		if _, ok := translateLangNames[targetLang]; ok && len([]rune(strings.TrimSpace(text))) >= 180 {
			return true
		}
	}
	return false
}

func translateSystemPrompt(targetLang string) string {
	name := translateLangNames[targetLang]
	return "Translate the following AI coding assistant reasoning trace into " + name + ".\n" +
		"Rules:\n" +
		"- If the text is already in " + name + ", return it verbatim.\n" +
		"- Keep all code identifiers, file paths, symbol names, CLI commands, flags, URLs, and inline/fenced code blocks verbatim.\n" +
		"- Preserve Markdown structure (headings, lists, bold, code fences) and paragraph breaks.\n" +
		"- Keep the translation concise, natural, and accurate for software engineering context.\n" +
		"- Output ONLY the translated text with no preamble, explanations, or extra quotes."
}

func summarizeSystemPrompt(targetLang string) string {
	switch targetLang {
	case "zh-CN":
		return "你是一位资深工程师，正在把 AI 编程助手的原始思考过程，用自然、直白、口语化的简体中文总结给同事看。\n" +
			"要求：\n" +
			"- 讲人话：像工程师当面口头同步进度一样自然（例如「排查下来问题出在……」「看了下代码发现……」「接下来打算先改……再跑测试」），彻底杜绝翻译腔、书面官话和机器人套话（不要出现「该思考过程表明」「助手认为」「综上所述」「值得注意的是」「为了确保」等表达）。\n" +
			"- 抓重点去废话：直接过滤掉自言自语、犹豫试探（如 Let me check / Wait / Okay）和车轱辘话，只提炼真正有信息量的发现、判断理由和下一步打算。\n" +
			"- 控制篇幅：原文较短（1–3 句）时，用 1–2 句极简大白话说清即可，不要强行分点；原文较长或包含多个判断/步骤时，用 1 句核心结论 + 2–4 条短要点（Markdown 列表），每条尽量不超过一句话。\n" +
			"- 保留技术精准度：文件名、函数/变量名、命令、参数、配置项、报错码原样保留并用反引号包裹（如 `convo_agy_input.go`、`flex: 1 1 auto`），不要生硬直译技术名词。\n" +
			"- 严格忠实原文，绝不脑补原文未提及的结论或动作。\n" +
			"- 只输出总结正文，不要加「总结：」「中文总结：」等任何前缀或多余引号。"
	case "zh-TW":
		return "你是一位資深工程師，正在把 AI 程式助手的原始思考過程，用自然、直白、口語化的繁體中文總結給同事看。\n" +
			"要求：\n" +
			"- 講人話：像工程師當面口頭同步進度一樣自然直白，杜絕翻譯腔、官話和機器人口吻（不要出現「該思考過程表明」「助手決定」「綜上所述」等說法）。\n" +
			"- 抓重點去廢話：過濾掉自言自語、猶豫試探和重複語句，只保留真正有資訊量的發現、判斷原因與下一步行動。\n" +
			"- 控制篇幅：原文較短（1–3 句）時用 1–2 句簡潔大白話說清；原文較長或包含多個步驟時，用 1 句核心結論 + 2–4 條精簡要點（Markdown 列表）。\n" +
			"- 保留技術精準度：檔名、函式/變數名、指令、參數與錯誤碼原樣保留並用反引號包裹。\n" +
			"- 嚴格忠實原文，不腦補未提及的內容。\n" +
			"- 只輸出總結正文，不加任何前綴或額外引號。"
	default:
		name := translateLangNames[targetLang]
		return "You are a senior software engineer summarizing an AI coding assistant's raw reasoning trace for a teammate in natural, plain-spoken " + name + ".\n" +
			"Rules:\n" +
			"- Speak like a real engineer giving a quick verbal update: clear, direct, and conversational. Avoid stiff robotic phrasing or literal translationese (never say 'The assistant is considering...', 'In summary...', or 'This reasoning trace shows...').\n" +
			"- Cut filler self-talk ('Let me check', 'Wait', 'Okay') and dead-end repetition; keep only the real findings, root causes, key decisions, and concrete next steps.\n" +
			"- Match length to complexity: for short thoughts (1-3 sentences), reply with 1-2 crisp sentences; for longer multi-step thoughts, write 1 short lead sentence followed by 2-4 concise Markdown bullets.\n" +
			"- Keep all file paths, code symbols, CLI commands, flags, and error codes verbatim in backticks.\n" +
			"- Never invent facts or actions not in the original trace.\n" +
			"- Output ONLY the summary with no preamble, heading, or extra quotes."
	}
}

func (s *Server) translateHTTP() *http.Client {
	if s.translateClient != nil {
		return s.translateClient
	}
	s.translateClientOnce.Do(func() {
		s.translateDefaultClient = newTranslateHTTPClient()
	})
	return s.translateDefaultClient
}

func (s *Server) callTranslateLLM(ctx context.Context, baseURL, model, apiKey, targetLang, mode, text string) (string, error) {
	if mode == "summarize" {
		return s.callChatLLM(ctx, baseURL, model, apiKey, summarizeSystemPrompt(targetLang), text, 0.25)
	}
	return s.callChatLLM(ctx, baseURL, model, apiKey, translateSystemPrompt(targetLang), text, 0.1)
}

func (s *Server) callChatLLM(ctx context.Context, baseURL, model, apiKey, systemPrompt, text string, temperature float64) (string, error) {
	out, retryNoReasoning, err := s.callChatLLMOnce(ctx, baseURL, model, apiKey, systemPrompt, text, temperature, true)
	if err != nil && retryNoReasoning {
		out, _, err = s.callChatLLMOnce(ctx, baseURL, model, apiKey, systemPrompt, text, temperature, false)
	}
	return out, err
}

func (s *Server) callChatLLMOnce(ctx context.Context, baseURL, model, apiKey, systemPrompt, text string, temperature float64, withReasoningOff bool) (string, bool, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": text},
		},
		"temperature": temperature,
	}
	if withReasoningOff {
		payload["reasoning_effort"] = "none"
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, translateEndpointURL(baseURL), bytes.NewReader(bodyBytes))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := s.translateHTTP().Do(req)
	if err != nil {
		return "", false, fmt.Errorf("translation request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, translateMaxRespBytes))
	if err != nil {
		return "", false, fmt.Errorf("reading translation response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 240 {
			msg = msg[:240]
		}
		// Some OpenAI-compatible proxies reject reasoning_effort with HTTP 400; retry once without it.
		if withReasoningOff && resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "reasoning_effort") {
			return "", true, fmt.Errorf("upstream status %d: %s", resp.StatusCode, msg)
		}
		return "", false, fmt.Errorf("upstream status %d: %s", resp.StatusCode, msg)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return "", false, errors.New("unexpected translation response format")
	}
	out := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if out == "" {
		return "", false, errors.New("empty translation response")
	}
	return out, false, nil
}

func (s *Server) openTranslateKey(cfg *store.TranslateConfig) (string, error) {
	if len(cfg.Ciphertext) == 0 {
		return "", nil
	}
	plain, err := s.vault.Open(vault.Sealed{
		Ciphertext: cfg.Ciphertext,
		WrappedKey: cfg.WrappedKey,
		KeyID:      cfg.KeyID,
	}, translateAAD(cfg.UserID))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

type translateConfigView struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`
	TargetLang string `json:"target_lang"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	HasKey     bool   `json:"has_key"`
}

func translateView(cfg *store.TranslateConfig) translateConfigView {
	mode := cfg.Mode
	if mode != "summarize" {
		mode = store.DefaultTranslateMode
	}
	return translateConfigView{
		Enabled:    cfg.Enabled,
		Mode:       mode,
		TargetLang: cfg.TargetLang,
		BaseURL:    cfg.BaseURL,
		Model:      cfg.Model,
		HasKey:     len(cfg.Ciphertext) > 0,
	}
}

// GET /api/me/translate
func (s *Server) apiGetTranslateConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.TranslateConfigOf(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, translateView(cfg))
}

// PUT /api/me/translate
func (s *Server) apiPutTranslateConfig(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Enabled    *bool   `json:"enabled"`
		Mode       *string `json:"mode"`
		TargetLang *string `json:"target_lang"`
		BaseURL    *string `json:"base_url"`
		Model      *string `json:"model"`
		APIKey     *string `json:"api_key"`
		ClearKey   bool    `json:"clear_key"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	uid := currentUser(r).ID
	cfg, err := s.store.TranslateConfigOf(r.Context(), uid)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if b.Enabled != nil {
		cfg.Enabled = *b.Enabled
	}
	if b.Mode != nil {
		mode, ok := normalizeTranslateMode(*b.Mode)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "unsupported translation mode")
			return
		}
		cfg.Mode = mode
	}
	if b.TargetLang != nil {
		lang := strings.TrimSpace(*b.TargetLang)
		if _, ok := translateLangNames[lang]; !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "unsupported target language")
			return
		}
		cfg.TargetLang = lang
	}
	if b.BaseURL != nil {
		u, err := normalizeTranslateBaseURL(*b.BaseURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		cfg.BaseURL = u
	}
	if b.Model != nil {
		m := strings.TrimSpace(*b.Model)
		if m == "" || len(m) > translateMaxModelLen {
			writeError(w, http.StatusBadRequest, "bad_request", "model is required and must be at most 128 characters")
			return
		}
		cfg.Model = m
	}
	if b.ClearKey {
		cfg.Ciphertext, cfg.WrappedKey, cfg.KeyID = nil, nil, ""
	} else if b.APIKey != nil {
		key := strings.TrimSpace(*b.APIKey)
		if len(key) > translateMaxKeyLen {
			writeError(w, http.StatusBadRequest, "bad_request", "API key is too long")
			return
		}
		if key != "" {
			sealed, err := s.vault.Seal([]byte(key), translateAAD(uid))
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			cfg.Ciphertext = sealed.Ciphertext
			cfg.WrappedKey = sealed.WrappedKey
			cfg.KeyID = sealed.KeyID
		}
	}
	if err := s.store.SaveTranslateConfig(r.Context(), cfg); err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, translateView(cfg))
}

// POST /api/me/translate/test
func (s *Server) apiTestTranslateConfig(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Mode       string `json:"mode"`
		TargetLang string `json:"target_lang"`
		BaseURL    string `json:"base_url"`
		Model      string `json:"model"`
		APIKey     string `json:"api_key"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &b); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	uid := currentUser(r).ID
	cfg, err := s.store.TranslateConfigOf(r.Context(), uid)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	mode := cfg.Mode
	if strings.TrimSpace(b.Mode) != "" {
		var ok bool
		mode, ok = normalizeTranslateMode(b.Mode)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "unsupported translation mode")
			return
		}
	}
	targetLang := cfg.TargetLang
	if strings.TrimSpace(b.TargetLang) != "" {
		targetLang = strings.TrimSpace(b.TargetLang)
	}
	if _, ok := translateLangNames[targetLang]; !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported target language")
		return
	}
	baseURL := cfg.BaseURL
	if strings.TrimSpace(b.BaseURL) != "" {
		baseURL, err = normalizeTranslateBaseURL(b.BaseURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	model := cfg.Model
	if strings.TrimSpace(b.Model) != "" {
		model = strings.TrimSpace(b.Model)
	}
	if model == "" || len(model) > translateMaxModelLen {
		writeError(w, http.StatusBadRequest, "bad_request", "model is required")
		return
	}
	apiKey := strings.TrimSpace(b.APIKey)
	if apiKey == "" {
		apiKey, err = s.openTranslateKey(cfg)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	sampleIn := "Checking the cache lookup before returning a response.\n\n- Verify `cache.get(key)` returns a fresh entry.\n- Fall back to the upstream API if the entry has expired."
	if targetLang == "en" {
		sampleIn = "先检查缓存命中情况，再返回结果。\n\n- 确认 `cache.get(key)` 返回的是有效条目。\n- 如果缓存已过期，则回退请求上游接口。"
	}
	t0 := time.Now()
	tctx, cancel := context.WithTimeout(r.Context(), translateUpstreamTime)
	defer cancel()
	out, err := s.callTranslateLLM(tctx, baseURL, model, apiKey, targetLang, mode, sampleIn)
	if err != nil {
		writeError(w, http.StatusBadGateway, "translate_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"mode":       mode,
		"original":   sampleIn,
		"sample":     out,
		"latency_ms": time.Since(t0).Milliseconds(),
	})
}

type translateThoughtResult struct {
	Hash   string `json:"hash"`
	Status string `json:"status"` // "ok", "skipped", "error"
	Text   string `json:"text,omitempty"`
	Cached bool   `json:"cached,omitempty"`
	Error  string `json:"error,omitempty"`
}

// POST /api/translate/thoughts
func (s *Server) apiTranslateThoughts(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Mode       string `json:"mode"`
		TargetLang string `json:"target_lang"`
		Items      []struct {
			Text string `json:"text"`
		} `json:"items"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(b.Items) == 0 || len(b.Items) > translateMaxItems {
		writeError(w, http.StatusBadRequest, "bad_request", "items must have between 1 and 16 entries")
		return
	}
	uid := currentUser(r).ID
	cfg, err := s.store.TranslateConfigOf(r.Context(), uid)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !cfg.Enabled {
		writeError(w, http.StatusConflict, "translate_disabled", "thought translation is disabled")
		return
	}
	mode := cfg.Mode
	if strings.TrimSpace(b.Mode) != "" {
		var ok bool
		mode, ok = normalizeTranslateMode(b.Mode)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "unsupported translation mode")
			return
		}
	}
	targetLang := cfg.TargetLang
	if strings.TrimSpace(b.TargetLang) != "" {
		targetLang = strings.TrimSpace(b.TargetLang)
	}
	if _, ok := translateLangNames[targetLang]; !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported target language")
		return
	}
	cacheLang := translateCacheLang(mode, targetLang)

	results := make([]translateThoughtResult, len(b.Items))
	needIdx := make([]int, 0, len(b.Items))
	needHashes := make([]string, 0, len(b.Items))
	seenHash := map[string]bool{}
	for i, it := range b.Items {
		txt := strings.TrimSpace(it.Text)
		if txt == "" || len(txt) > translateMaxTextBytes {
			results[i] = translateThoughtResult{Status: "skipped"}
			continue
		}
		h := translateHash(txt)
		results[i].Hash = h
		if !needsThoughtProcessing(txt, targetLang, mode) {
			results[i].Status = "skipped"
			continue
		}
		needIdx = append(needIdx, i)
		if !seenHash[h] {
			seenHash[h] = true
			needHashes = append(needHashes, h)
		}
	}

	if len(needHashes) > 0 {
		cached, err := s.store.CachedThoughtTranslations(r.Context(), uid, cacheLang, needHashes)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		var misses []int
		for _, i := range needIdx {
			h := results[i].Hash
			if tr, ok := cached[h]; ok {
				results[i] = translateThoughtResult{Hash: h, Status: "ok", Text: tr, Cached: true}
			} else {
				misses = append(misses, i)
			}
		}

		if len(misses) > 0 {
			apiKey, err := s.openTranslateKey(cfg)
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			baseURL, err := normalizeTranslateBaseURL(cfg.BaseURL)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			sem := s.translateFlight.sem(uid)
			var wg sync.WaitGroup
			for _, idx := range misses {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					txt := strings.TrimSpace(b.Items[i].Text)
					h := results[i].Hash
					flightKey := fmt.Sprintf("%d\x00%s\x00%s", uid, cacheLang, h)
					tr, err := s.translateFlight.do(flightKey, func() (string, error) {
						select {
						case sem <- struct{}{}:
							defer func() { <-sem }()
						case <-r.Context().Done():
							return "", r.Context().Err()
						}
						tctx, cancel := context.WithTimeout(r.Context(), translateUpstreamTime)
						defer cancel()
						out, err := s.callTranslateLLM(tctx, baseURL, cfg.Model, apiKey, targetLang, mode, txt)
						if err != nil {
							return "", err
						}
						_ = s.store.PutThoughtTranslation(context.Background(), uid, h, cacheLang, out)
						return out, nil
					})
					if err != nil {
						results[i] = translateThoughtResult{Hash: h, Status: "error", Error: err.Error()}
					} else {
						results[i] = translateThoughtResult{Hash: h, Status: "ok", Text: tr}
					}
				}(idx)
			}
			wg.Wait()
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":        mode,
		"target_lang": targetLang,
		"results":     results,
	})
}
