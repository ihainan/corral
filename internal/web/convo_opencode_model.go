package web

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// OpenCode 2 mode, model, and reasoning-variant switching via the local
// `opencode api` HTTP API surface:
//   - GET  /api/model                      -> list models + reasoning variants
//   - GET  /api/provider                   -> list connected/configured providers
//   - GET  /api/session/{id}               -> current session agent & model ref
//   - POST /api/session/{id}/agent         -> switch primary agent ("build" | "plan")
//   - POST /api/session/{id}/model         -> switch model & optional variant

var (
	ocModes      = map[string]bool{"build": true, "plan": true}
	ocProviderRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	ocModelIDRe  = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,120}$`)
	ocVariantRe  = regexp.MustCompile(`^(?:default|none|minimal|low|medium|high|xhigh|max)$`)
)

const (
	ocModelCacheTTL = 10 * time.Minute
)

type ocModelItem struct {
	ID         string   `json:"id"`
	ProviderID string   `json:"providerID"`
	Name       string   `json:"name"`
	Variants   []string `json:"variants,omitempty"`
}

type ocModelCacheEntry struct {
	at        time.Time
	providers map[string]bool
	models    []ocModelItem
}

var ocModelCache = struct {
	mu sync.Mutex
	m  map[int64]ocModelCacheEntry
}{m: map[int64]ocModelCacheEntry{}}

func ocAPICommand(method, path, body string) string {
	cmd := `PATH="$HOME/.opencode/bin:$HOME/.local/bin:$HOME/bin:/usr/local/bin:$PATH" opencode api ` +
		sshx.QuoteShell(method) + ` ` + sshx.QuoteShell(path)
	if body != "" {
		cmd += ` -d ` + sshx.QuoteShell(body)
	}
	return cmd
}

func parseOpenCodeProviders(raw []byte) map[string]bool {
	var wrap struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	out := map[string]bool{}
	if json.Unmarshal(raw, &wrap) == nil {
		for _, p := range wrap.Data {
			if p.ID != "" {
				out[p.ID] = true
			}
		}
	}
	return out
}

func parseOpenCodeModels(raw []byte) []ocModelItem {
	var wrap struct {
		Data []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Name       string `json:"name"`
			Variants   []struct {
				ID string `json:"id"`
			} `json:"variants"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return nil
	}
	out := make([]ocModelItem, 0, len(wrap.Data))
	seen := map[string]bool{}
	for _, m := range wrap.Data {
		if m.ID == "" || m.ProviderID == "" {
			continue
		}
		full := m.ProviderID + "/" + m.ID
		if seen[full] {
			continue
		}
		seen[full] = true
		var vars []string
		for _, v := range m.Variants {
			id := strings.ToLower(strings.TrimSpace(v.ID))
			if id != "" && ocVariantRe.MatchString(id) && !slices.Contains(vars, id) {
				vars = append(vars, id)
			}
		}
		out = append(out, ocModelItem{
			ID:         m.ID,
			ProviderID: m.ProviderID,
			Name:       strings.TrimSpace(m.Name),
			Variants:   vars,
		})
	}
	return out
}

type ocSessionRef struct {
	Agent      string
	ProviderID string
	ModelID    string
	Variant    string
}

func parseOpenCodeSession(raw []byte) (ocSessionRef, bool) {
	var wrap struct {
		Data struct {
			Agent string `json:"agent"`
			Model *struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
				Variant    string `json:"variant"`
			} `json:"model"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return ocSessionRef{}, false
	}
	ref := ocSessionRef{Agent: wrap.Data.Agent}
	if wrap.Data.Model != nil {
		ref.ProviderID = wrap.Data.Model.ProviderID
		ref.ModelID = wrap.Data.Model.ID
		ref.Variant = wrap.Data.Model.Variant
	}
	return ref, true
}

func splitOpenCodeModelRef(target string) (providerID, modelID string, ok bool) {
	t := strings.TrimSpace(target)
	i := strings.IndexByte(t, '/')
	if i <= 0 || i == len(t)-1 {
		return "", "", false
	}
	p, id := t[:i], t[i+1:]
	if !ocProviderRe.MatchString(p) || !ocModelIDRe.MatchString(id) {
		return "", "", false
	}
	return p, id, true
}

// ocModelOpts formats OpenCode models for Corral's model picker, keeping the
// current session's provider and OpenCode Go / OpenCode models at the top so
// the list stays focused and easy to pick from.
func ocModelOpts(models []ocModelItem, providers map[string]bool, cur ocSessionRef) []modelOpt {
	curFull := ""
	if cur.ProviderID != "" && cur.ModelID != "" {
		curFull = cur.ProviderID + "/" + cur.ModelID
	}
	rank := func(prov string) int {
		switch {
		case cur.ProviderID != "" && prov == cur.ProviderID:
			return 0
		case prov == "opencode-go":
			return 1
		case prov == "opencode":
			return 2
		case providers[prov]:
			return 3
		default:
			return 4
		}
	}
	// Include models from connected providers or the current session's provider.
	// If provider list was empty, keep all models.
	filtered := make([]ocModelItem, 0, len(models))
	for _, m := range models {
		if len(providers) > 0 && !providers[m.ProviderID] && m.ProviderID != cur.ProviderID {
			continue
		}
		// Keep openrouter out unless the session itself is already on openrouter
		// (openrouter exposes 300+ models and would drown out opencode-go/devin).
		if m.ProviderID == "openrouter" && cur.ProviderID != "openrouter" {
			continue
		}
		filtered = append(filtered, m)
	}
	if len(filtered) == 0 {
		filtered = append(filtered, models...)
	}
	slices.SortStableFunc(filtered, func(a, b ocModelItem) int {
		ra, rb := rank(a.ProviderID), rank(b.ProviderID)
		if ra != rb {
			return ra - rb
		}
		return strings.Compare(strings.ToLower(a.Name+" "+a.ID), strings.ToLower(b.Name+" "+b.ID))
	})
	if len(filtered) > 80 {
		filtered = filtered[:80]
	}
	opts := make([]modelOpt, 0, len(filtered)+1)
	seenCur := false
	for _, m := range filtered {
		full := m.ProviderID + "/" + m.ID
		isCur := curFull != "" && strings.EqualFold(full, curFull)
		if isCur {
			seenCur = true
		}
		desc := m.Name
		if desc == "" {
			desc = m.ProviderID
		}
		opts = append(opts, modelOpt{
			Label:   full,
			Desc:    desc,
			Current: isCur,
			Efforts: m.Variants,
		})
	}
	if curFull != "" && !seenCur {
		opts = append([]modelOpt{{Label: curFull, Desc: cur.ProviderID, Current: true}}, opts...)
	}
	return opts
}

func (s *Server) ocFetchModels(ctx context.Context, sess *term.Session) ([]ocModelItem, map[string]bool, error) {
	ocModelCache.mu.Lock()
	ent, ok := ocModelCache.m[sess.HostID]
	ocModelCache.mu.Unlock()
	if ok && time.Since(ent.at) < ocModelCacheTTL && len(ent.models) > 0 {
		return ent.models, ent.providers, nil
	}
	mctx, mcancel := context.WithTimeout(ctx, 10*time.Second)
	defer mcancel()
	mOut, err := sshx.Output(mctx, sess.Client(), ocAPICommand("GET", "/api/model", ""), 2<<20)
	if err != nil {
		return nil, nil, err
	}
	models := parseOpenCodeModels(mOut)
	pOut, perr := sshx.Output(mctx, sess.Client(), ocAPICommand("GET", "/api/provider", ""), 256<<10)
	var provs map[string]bool
	if perr == nil {
		provs = parseOpenCodeProviders(pOut)
	}
	if len(models) > 0 {
		ocModelCache.mu.Lock()
		ocModelCache.m[sess.HostID] = ocModelCacheEntry{at: time.Now(), providers: provs, models: models}
		ocModelCache.mu.Unlock()
	}
	return models, provs, nil
}

func (s *Server) ocReadSessionRef(ctx context.Context, sess *term.Session, sid string) (ocSessionRef, error) {
	sctx, scancel := context.WithTimeout(ctx, 6*time.Second)
	defer scancel()
	out, err := sshx.Output(sctx, sess.Client(), ocAPICommand("GET", "/api/session/"+sid, ""), 256<<10)
	if err != nil {
		return ocSessionRef{}, err
	}
	ref, _ := parseOpenCodeSession(out)
	return ref, nil
}

func (s *Server) convoOpenCodeMode(w http.ResponseWriter, ctx context.Context, sess *term.Session, sid, target string, outcome, reason *string, release func()) {
	defer release()
	if !ocSessionID.MatchString(sid) || !ocModes[target] {
		*reason = "bad_request"
		writeError(w, http.StatusBadRequest, "bad_request", "invalid OpenCode session or mode")
		return
	}
	body, _ := json.Marshal(map[string]string{"agent": target})
	rctx, rcancel := context.WithTimeout(ctx, 8*time.Second)
	defer rcancel()
	if _, err := sshx.Output(rctx, sess.Client(), ocAPICommand("POST", "/api/session/"+sid+"/agent", string(body)), 64<<10); err != nil {
		*reason = "host_error"
		writeError(w, http.StatusBadGateway, "host_error", "failed to switch OpenCode mode")
		return
	}
	*outcome = "typed"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": target})
}

func (s *Server) convoOpenCodeModels(w http.ResponseWriter, ctx context.Context, sess *term.Session, sid string, outcome, reason *string, release func()) {
	defer release()
	if !ocSessionID.MatchString(sid) {
		*reason = "bad_request"
		writeError(w, http.StatusBadRequest, "bad_request", "invalid OpenCode session")
		return
	}
	models, provs, err := s.ocFetchModels(ctx, sess)
	if err != nil || len(models) == 0 {
		*reason = "no_models"
		writeError(w, http.StatusBadGateway, "host_error", "could not list OpenCode models")
		return
	}
	cur, _ := s.ocReadSessionRef(ctx, sess, sid)
	*outcome = "typed"
	writeJSON(w, http.StatusOK, map[string]any{"models": ocModelOpts(models, provs, cur)})
}

func (s *Server) convoOpenCodeModel(w http.ResponseWriter, ctx context.Context, sess *term.Session, sid, target, effort string, outcome, reason *string, release func()) {
	defer release()
	if !ocSessionID.MatchString(sid) {
		*reason = "bad_request"
		writeError(w, http.StatusBadRequest, "bad_request", "invalid OpenCode session")
		return
	}
	prov, id, ok := splitOpenCodeModelRef(target)
	if !ok {
		*reason = "bad_request"
		writeError(w, http.StatusBadRequest, "bad_request", "model must be provider/id")
		return
	}
	mRef := map[string]string{"providerID": prov, "id": id}
	eff := strings.ToLower(strings.TrimSpace(effort))
	if eff != "" && eff != "default" {
		if !ocVariantRe.MatchString(eff) {
			*reason = "bad_request"
			writeError(w, http.StatusBadRequest, "bad_request", "invalid OpenCode variant")
			return
		}
		mRef["variant"] = eff
	}
	body, _ := json.Marshal(map[string]any{"model": mRef})
	rctx, rcancel := context.WithTimeout(ctx, 8*time.Second)
	defer rcancel()
	if _, err := sshx.Output(rctx, sess.Client(), ocAPICommand("POST", "/api/session/"+sid+"/model", string(body)), 64<<10); err != nil {
		*reason = "host_error"
		writeError(w, http.StatusBadGateway, "host_error", "failed to switch OpenCode model")
		return
	}
	*outcome = "typed"
	res := map[string]any{"ok": true, "model": prov + "/" + id}
	if eff != "" {
		res["effort"] = eff
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) convoOpenCodeEffort(w http.ResponseWriter, ctx context.Context, sess *term.Session, sid, effort string, outcome, reason *string, release func()) {
	defer release()
	if !ocSessionID.MatchString(sid) {
		*reason = "bad_request"
		writeError(w, http.StatusBadRequest, "bad_request", "invalid OpenCode session")
		return
	}
	eff := strings.ToLower(strings.TrimSpace(effort))
	if !ocVariantRe.MatchString(eff) {
		*reason = "bad_request"
		writeError(w, http.StatusBadRequest, "bad_request", "invalid OpenCode variant")
		return
	}
	cur, err := s.ocReadSessionRef(ctx, sess, sid)
	if err != nil || cur.ProviderID == "" || cur.ModelID == "" {
		*reason = "no_model"
		writeError(w, http.StatusConflict, "unsupported", "current OpenCode model could not be read")
		return
	}
	mRef := map[string]string{"providerID": cur.ProviderID, "id": cur.ModelID}
	if eff != "default" {
		mRef["variant"] = eff
	}
	body, _ := json.Marshal(map[string]any{"model": mRef})
	rctx, rcancel := context.WithTimeout(ctx, 8*time.Second)
	defer rcancel()
	if _, err := sshx.Output(rctx, sess.Client(), ocAPICommand("POST", "/api/session/"+sid+"/model", string(body)), 64<<10); err != nil {
		*reason = "host_error"
		writeError(w, http.StatusBadGateway, "host_error", "failed to switch OpenCode thinking variant")
		return
	}
	*outcome = "typed"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "effort": eff})
}
