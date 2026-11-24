package web

import (
	"bytes"
	"errors"
	"image"
	_ "image/jpeg" // (decoders for image.DecodeConfig: the two kinds a picture may be)
	_ "image/png"
	"io"
	"net/http"
	"strconv"

	"github.com/ihainan/corral/internal/store"
)

// An account's own picture (doc UI item 6, 11-24). The browser squares it and
// makes it small (256, JPEG) before it is sent; here it is only checked: a PNG
// or a JPEG by its own bytes, not by what the request says, small in size and
// in pixels. Served back with that type and nothing to run (nosniff, a CSP of
// none), so a file made to look like a picture is never read as a page.

const avatarMaxSide = 512

// PUT /api/me/avatar: the picture's bytes as the body.
func (s *Server) apiPutAvatar(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, store.MaxAvatarBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the picture")
		return
	}
	if len(body) > store.MaxAvatarBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the picture is too large")
		return
	}
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || (kind != "png" && kind != "jpeg") {
		writeError(w, http.StatusUnprocessableEntity, "not_image", "not a PNG or JPEG picture")
		return
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > avatarMaxSide || cfg.Height > avatarMaxSide {
		writeError(w, http.StatusUnprocessableEntity, "bad_size", "the picture must be at most 512 pixels a side")
		return
	}
	// (Its pixels read whole too: a header alone can be well formed over bytes that are not.)
	if _, _, err := image.Decode(bytes.NewReader(body)); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "not_image", "not a PNG or JPEG picture")
		return
	}
	u := currentUser(r)
	at, err := s.store.SetAvatar(r.Context(), u.ID, "image/"+kind, body)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	u.AvatarAt = at
	writeJSON(w, http.StatusOK, viewMe(u))
}

// DELETE /api/me/avatar
func (s *Server) apiDeleteAvatar(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if err := s.store.DeleteAvatar(r.Context(), u.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	u.AvatarAt = 0
	writeJSON(w, http.StatusOK, viewMe(u))
}

// GET /api/users/{id}/avatar: any signed-in account's, as its name is shown
// to the others (an administrator's list, who watches a shared session).
func (s *Server) apiGetAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, "not_found", "no such picture")
		return
	}
	mime, img, at, err := s.store.Avatar(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such picture")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// (Its address names its time, ?v=: one asked for again is the same picture; a new one has a new address.)
	if r.URL.Query().Get("v") == strconv.FormatInt(at, 10) {
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "private, no-cache")
	}
	h.Set("Content-Length", strconv.Itoa(len(img)))
	_, _ = w.Write(img)
}
