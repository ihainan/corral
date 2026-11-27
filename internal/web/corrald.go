package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/codeserver"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

type webSSHProvider struct {
	s *Server
}

func (p *webSSHProvider) AcquireSSH(ctx context.Context, ownerID, hostID int64) (*ssh.Client, func(), string, string, error) {
	h, err := p.s.store.Host(ctx, ownerID, hostID)
	if err != nil {
		return nil, nil, "", "", err
	}
	cfg := hostConfig(h)
	// Reuse an open terminal SSH connection for this host if its settings still match.
	for _, rs := range p.s.terms.Running(ownerID, hostID) {
		if rs.HostConfig == cfg && rs.Client() != nil {
			fp := ""
			if k, kerr := p.s.store.HostKey(ctx, ownerID, hostID); kerr == nil {
				fp = k.Fingerprint
			}
			if fp != "" {
				_ = p.s.store.RecordSSHVerified(ctx, hostID, fp, h.Username)
			}
			return rs.Client(), func() {}, fp, h.Username, nil
		}
	}
	// Otherwise dial using stored credentials + pinned host key.
	t, err := p.s.storedTarget(ctx, h)
	if err != nil {
		return nil, nil, "", "", gw.ErrNotConnected
	}
	dialed, err := sshx.Dial(ctx, p.s.guard, *t)
	if err != nil {
		return nil, nil, "", "", gw.ErrNotConnected
	}
	fp := ""
	if dialed.SeenKey != nil {
		fp = dialed.SeenKey.Fingerprint
		_ = p.s.store.RecordSSHVerified(ctx, hostID, fp, h.Username)
	}
	return dialed.Client, func() { _ = dialed.Client.Close() }, fp, h.Username, nil
}

func (p *webSSHProvider) HostBinding(ctx context.Context, ownerID, hostID int64) (string, string, error) {
	h, err := p.s.store.Host(ctx, ownerID, hostID)
	if err != nil {
		return "", "", err
	}
	fp := ""
	if k, kerr := p.s.store.HostKey(ctx, ownerID, hostID); kerr == nil {
		fp = k.Fingerprint
	}
	return h.Username, fp, nil
}

func (p *webSSHProvider) UserLabel(ctx context.Context, userID int64) string {
	u, err := p.s.store.UserByID(ctx, userID)
	if err != nil {
		return ""
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func (s *Server) initCorrald() {
	var dist *gw.Dist
	if s.cfg != nil && s.cfg.CorraldDir != "" {
		d, err := gw.LoadDist(s.cfg.CorraldDir)
		if err != nil {
			if s.log != nil {
				s.log.Info("corrald binaries unavailable; helper daemon feature disabled", "dir", s.cfg.CorraldDir, "err", err)
			}
		} else {
			dist = d
		}
	}
	gwID := ""
	adminDisabled := false
	if s.cfg != nil {
		gwID = s.cfg.GatewayID
		if gwID == "" && s.cfg.PublicURL != nil {
			gwID = s.cfg.PublicURL.Hostname()
		}
		adminDisabled = s.cfg.CorraldAdminDisabled
	}
	s.corrald = gw.NewManager(gw.ManagerConfig{
		Store:          s.store,
		Dist:           dist,
		GatewayID:      gwID,
		GatewayVersion: Version,
		AdminDisabled:  adminDisabled,
		Log:            s.log,
		SSH:            &webSSHProvider{s: s},
	})
}

// CorraldManager exposes the gateway manager (for T3 integration and tests).
func (s *Server) CorraldManager() *gw.Manager {
	return s.corrald
}

// CorraldClient is the unified fallback entrypoint (spec §9) for stage 1
// features to obtain a connected corrald client when the given capability is
// healthy, or (nil, false) to fall back to the standard SSH path.
func (s *Server) CorraldClient(userID, hostID int64, capName string) (*gw.Client, bool) {
	if s.corrald == nil {
		return nil, false
	}
	return s.corrald.ClientForCap(userID, hostID, capName)
}

// notifyCorraldConnected records successful SSH verification and triggers
// corrald's asynchronous OnHostConnected lifecycle reconciliation/attachment.
func (s *Server) notifyCorraldConnected(ownerID, hostID int64, corralUser, sshUser, fp string, c *ssh.Client) {
	if fp != "" && sshUser != "" {
		_ = s.store.RecordSSHVerified(context.Background(), hostID, fp, sshUser)
	}
	if mgr := s.corrald; mgr != nil && mgr.Available() && c != nil {
		go func(m *gw.Manager, uid, hid int64, uname, fpStr, sUser string, client *ssh.Client) {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			m.OnHostConnected(cctx, uid, hid, uname, fpStr, sUser, client)
		}(mgr, ownerID, hostID, corralUser, fp, sshUser, c)
	}
}

func (s *Server) writeCorraldError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, gw.ErrUnavailable):
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
	case errors.Is(err, gw.ErrAdminDisabled):
		writeError(w, http.StatusForbidden, "corrald_disabled", "helper daemon is disabled by the administrator")
	case errors.Is(err, gw.ErrManifestRequired):
		writeError(w, http.StatusBadRequest, "bad_request", "manifest_hash is required")
	case errors.Is(err, gw.ErrManifestChanged):
		writeError(w, http.StatusConflict, "manifest_changed", "helper daemon manifest changed; reload and review details")
	case errors.Is(err, gw.ErrUnknownResult):
		writeError(w, http.StatusConflict, "corrald_unknown_result", "write operation result unknown; do not retry blindly")
	case errors.Is(err, store.ErrOpInProgress):
		writeError(w, http.StatusConflict, "op_in_progress", "another helper daemon operation is already in progress")
	case errors.Is(err, gw.ErrCannotCancel):
		writeError(w, http.StatusConflict, "cannot_cancel", "this operation can no longer be cancelled")
	case errors.Is(err, gw.ErrInvalidState):
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
	case errors.Is(err, gw.ErrNotConnected):
		writeError(w, http.StatusBadGateway, "corrald_not_connected", "cannot connect to the host")
	default:
		s.internalError(w, r, err)
	}
}

// GET /api/corrald/readme
func (s *Server) apiCorraldReadme(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	content, hash, err := s.corrald.Readme()
	if err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content":       content,
		"manifest_hash": hash,
		"format":        "text",
	})
}

// GET /api/hosts/{id}/corrald
func (s *Server) apiGetHostCorrald(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	st, err := s.corrald.Status(r.Context(), currentUser(r).ID, h.ID)
	if err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// POST /api/hosts/{id}/corrald/install
func (s *Server) apiInstallHostCorrald(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	var b struct {
		ManifestHash string `json:"manifest_hash"`
		Rebind       bool   `json:"rebind"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &b); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	u := currentUser(r)
	op, err := s.corrald.StartInstall(r.Context(), u.ID, h.ID, u.Username, strings.TrimSpace(b.ManifestHash), b.Rebind)
	if err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	dist := s.corrald.Dist()
	detail := map[string]any{"op_id": op.ID, "rebind": b.Rebind}
	if dist != nil {
		detail["version"] = dist.Version
		detail["release"] = dist.Release
		detail["manifest_hash"] = dist.ManifestHash
	}
	s.audit(r, "corrald.install", hostTarget(h), detail)
	writeJSON(w, http.StatusAccepted, op)
}

// POST /api/hosts/{id}/corrald/cancel
func (s *Server) apiCancelHostCorrald(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	op, err := s.corrald.CancelInstall(r.Context(), currentUser(r).ID, h.ID)
	if err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

// POST /api/hosts/{id}/corrald/pause
func (s *Server) apiPauseHostCorrald(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	if err := s.corrald.Pause(r.Context(), u.ID, h.ID, u.Username); err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	s.audit(r, "corrald.pause", hostTarget(h), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/hosts/{id}/corrald/resume
func (s *Server) apiResumeHostCorrald(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	if err := s.corrald.Resume(r.Context(), u.ID, h.ID, u.Username); err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	s.audit(r, "corrald.resume", hostTarget(h), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/hosts/{id}/corrald/remove
func (s *Server) apiRemoveHostCorrald(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	op, err := s.corrald.StartRemove(r.Context(), currentUser(r).ID, h.ID)
	if err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	s.audit(r, "corrald.remove", hostTarget(h), map[string]any{"op_id": op.ID})
	writeJSON(w, http.StatusAccepted, op)
}

// POST /api/hosts/{id}/corrald/update
func (s *Server) apiUpdateHostCorrald(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	var b struct {
		ManifestHash string `json:"manifest_hash"`
		Replace      bool   `json:"replace"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &b); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	u := currentUser(r)
	op, err := s.corrald.StartUpdate(r.Context(), u.ID, h.ID, u.Username, strings.TrimSpace(b.ManifestHash), b.Replace)
	if err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	s.audit(r, "corrald.update", hostTarget(h), map[string]any{"op_id": op.ID, "replace": b.Replace})
	writeJSON(w, http.StatusAccepted, op)
}

// POST /api/hosts/{id}/corrald/retry
func (s *Server) apiRetryHostCorrald(w http.ResponseWriter, r *http.Request) {
	if s.corrald == nil || !s.corrald.Available() {
		writeError(w, http.StatusNotFound, "corrald_unavailable", "helper daemon is not available on this gateway")
		return
	}
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	op, err := s.corrald.Retry(r.Context(), u.ID, h.ID, u.Username)
	if err != nil {
		s.writeCorraldError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

// hasActiveCorraldAuth reports whether hostID currently has an active corrald
// authorization that could be carried over on SSH host key rotation.
func (s *Server) hasActiveCorraldAuth(ctx context.Context, ownerID, hostID int64) bool {
	if s.corrald == nil || !s.corrald.Available() {
		return false
	}
	ch, err := s.store.CorraldHost(ctx, ownerID, hostID)
	if err != nil {
		return false
	}
	switch ch.State {
	case "", gw.StateOff, gw.StateRebind, gw.StateRemoving, gw.StateRemovePending:
		return false
	default:
		return true
	}
}

// deleteHostCodeCleanup synchronously stops and/or removes VS Code on an
// already-established SSH connection during DELETE /api/hosts/{id}.
func (s *Server) deleteHostCodeCleanup(ctx context.Context, c *ssh.Client, stopVSCode, removeVSCode bool) map[string]any {
	res := map[string]any{}
	if !stopVSCode && !removeVSCode {
		return nil
	}
	if c == nil {
		res["error"] = "unreachable"
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, sock, err := s.probeCode(cctx, c)
	if err != nil {
		res["error"] = err.Error()
		return res
	}
	if removeVSCode {
		fields, _, rerr := s.codeRun(cctx, c, codeserver.RemoveCommand(sock, newTaskID()), 20*time.Second, sshx.ExecOptions{})
		if rerr != nil || fields["result"] != "ok" {
			if rerr != nil {
				res["error"] = rerr.Error()
			} else {
				res["error"] = fields["error"]
			}
			return res
		}
		res["stopped"] = true
		res["removed"] = true
		return res
	}
	fields, _, serr := s.codeRun(cctx, c, codeserver.ForceStopCommand(sock, newTaskID()), 20*time.Second, sshx.ExecOptions{})
	if serr != nil || fields["result"] != "ok" {
		if serr != nil {
			res["error"] = serr.Error()
		} else {
			res["error"] = fields["error"]
		}
		return res
	}
	res["stopped"] = true
	return res
}
