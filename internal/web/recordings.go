package web

// Session recordings: when the deployment turns recording on, each session's
// output is written to DataDir/recordings/<session id>.cast.gz (see package
// record) and listed in the database. Owners see their own recordings,
// admins everyone's; only admins delete them. Watching or downloading one is
// audited.

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/record"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

var recordingIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func (s *Server) recordingsDir() string { return filepath.Join(s.cfg.DataDir, "recordings") }

func (s *Server) recordingPath(id string) string {
	return filepath.Join(s.recordingsDir(), id+".cast.gz")
}

// recState tracks the recordings being made, so shutdown waits for them and
// pruning tells finished-but-unrecorded ones from live ones.
type recState struct {
	mu     sync.Mutex
	active map[string]bool
	wg     sync.WaitGroup
	stop   chan struct{}
	done   chan struct{}
}

// startRecordings closes recordings left open (by a gateway that stopped
// while making them) and starts pruning old ones, whether or not recording
// is on now; new sessions are recorded only when it is.
func (s *Server) startRecordings() error {
	if err := os.MkdirAll(s.recordingsDir(), 0o700); err != nil {
		return err
	}
	s.rec = &recState{active: map[string]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	s.finishStale()
	if s.cfg.Recording {
		s.terms.NewRecorder = s.newRecorder
	}
	go s.pruneRecordings()
	return nil
}

// stopRecordings waits (briefly) for recordings being written to complete,
// after the sessions were closed.
func (s *Server) stopRecordings() {
	if s.rec == nil {
		return
	}
	select {
	case <-s.rec.stop:
		return // stopped already
	default:
		close(s.rec.stop)
	}
	// Pruning and the recordings being written, within one bound.
	finished := make(chan struct{})
	go func() {
		<-s.rec.done
		s.rec.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(8 * time.Second):
		s.log.Warn("gave up waiting for recordings to be written")
	}
}

// finishStale marks recordings that say they are being made, but are not,
// as ended: cut short, size unknown.
func (s *Server) finishStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids, err := s.store.UnfinishedRecordings(ctx)
	if err != nil {
		return
	}
	for _, id := range ids {
		s.rec.mu.Lock()
		live := s.rec.active[id]
		s.rec.mu.Unlock()
		if !live {
			_ = s.store.FinishStaleRecording(ctx, id)
		}
	}
}

// newRecorder is the terminal manager's hook for each new session. It
// returns at once: the database row and the file are made on the
// recorder's own goroutine. A recording that cannot be made is logged and
// audited; the session goes on without one.
func (s *Server) newRecorder(sess *term.Session) term.Recorder {
	if !recordingIDRE.MatchString(sess.ID) {
		return nil
	}
	info := sess.Info()
	started := time.Now().Unix()
	created := false
	s.rec.mu.Lock()
	s.rec.active[sess.ID] = true
	s.rec.wg.Add(1)
	s.rec.mu.Unlock()
	open := func() (io.WriteCloser, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		owner := ""
		if u, err := s.store.UserByID(ctx, sess.OwnerID); err == nil {
			owner = u.Username
		}
		row := &store.Recording{ID: sess.ID, OwnerID: sess.OwnerID, OwnerName: owner, HostID: sess.HostID, HostName: sess.HostName, StartedAt: started}
		if err := s.store.CreateRecording(ctx, row); err != nil {
			return nil, err
		}
		created = true
		f, err := os.OpenFile(s.recordingPath(sess.ID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = s.store.DeleteRecording(ctx, sess.ID)
			created = false
		}
		return f, err
	}
	done := func(res record.Result) {
		defer func() {
			s.rec.mu.Lock()
			delete(s.rec.active, sess.ID)
			s.rec.mu.Unlock()
			s.rec.wg.Done()
		}()
		if !created {
			s.log.Error("cannot record session", "session", sess.ID, "err", res.Err)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.store.Audit(ctx, &sess.OwnerID, "", "recording.failed", sess.HostName, "", map[string]any{"session": sess.ID})
			return
		}
		if res.Err != nil {
			s.log.Error("recording write failed", "session", sess.ID, "err", res.Err)
		}
		// A few tries; one that never succeeds is caught by finishStale.
		for i := 0; i < 3; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.store.FinishRecording(ctx, sess.ID, res.Bytes, res.Truncated || res.Err != nil, res.Gaps)
			cancel()
			if err == nil {
				return
			}
			s.log.Error("cannot finish recording", "session", sess.ID, "err", err)
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}
	return record.New(info.Cols, info.Rows, sess.HostName, s.cfg.RecordingMaxBytes, open, done)
}

func (s *Server) pruneRecordings() {
	defer close(s.rec.done)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.rec.stop:
			return
		case <-t.C:
			s.finishStale()
			s.pruneRecordingsOnce()
		}
	}
}

// pruneRecordingsOnce deletes recordings older than the retention period.
func (s *Server) pruneRecordingsOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cutoff := time.Now().AddDate(0, 0, -s.cfg.RecordingDays).Unix()
	ids, err := s.store.RecordingsBefore(ctx, cutoff)
	if err != nil {
		return
	}
	for _, id := range ids {
		if err := os.Remove(s.recordingPath(id)); err == nil || errors.Is(err, os.ErrNotExist) {
			_ = s.store.DeleteRecording(ctx, id)
		}
	}
}

// visibleRecording returns a recording the user may see, or writes the error.
func (s *Server) visibleRecording(w http.ResponseWriter, r *http.Request) (*store.Recording, bool) {
	id := r.PathValue("id")
	u := currentUser(r)
	rec, err := s.store.RecordingByID(r.Context(), id)
	if err != nil || !recordingIDRE.MatchString(id) || (rec.OwnerID != u.ID && !u.IsAdmin()) {
		writeError(w, http.StatusNotFound, "not_found", "no such recording")
		return nil, false
	}
	return rec, true
}

func (s *Server) apiListRecordings(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	owner := u.ID
	if u.IsAdmin() {
		owner = 0
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	list, err := s.store.ListRecordings(r.Context(), owner, before, r.URL.Query().Get("after_id"), 100)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recordings": list, "enabled": s.cfg.Recording, "days": s.cfg.RecordingDays})
}

func (s *Server) apiGetRecording(w http.ResponseWriter, r *http.Request) {
	if rec, ok := s.visibleRecording(w, r); ok {
		writeJSON(w, http.StatusOK, rec)
	}
}

// apiRecordingCast sends the recording as asciicast text, to play or (with
// ?download=1) to save.
func (s *Server) apiRecordingCast(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.visibleRecording(w, r)
	if !ok {
		return
	}
	rc, err := record.Open(s.recordingPath(rec.ID))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "the recording file is missing")
		return
	}
	defer rc.Close()
	download := r.URL.Query().Get("download") == "1"
	s.audit(r, "recording.view", rec.HostName, map[string]any{"recording": rec.ID, "owner": rec.OwnerName, "download": download})
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if download {
		name := rec.HostName + "-" + time.Unix(rec.StartedAt, 0).UTC().Format("20060102-150405") + ".cast"
		if d := mime.FormatMediaType("attachment", map[string]string{"filename": name}); d != "" {
			h.Set("Content-Disposition", d)
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (s *Server) apiDeleteRecording(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.visibleRecording(w, r)
	if !ok {
		return
	}
	if rec.EndedAt == 0 {
		writeError(w, http.StatusConflict, "recording_active", "the session is still being recorded")
		return
	}
	if err := os.Remove(s.recordingPath(rec.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.DeleteRecording(r.Context(), rec.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "recording.delete", rec.HostName, map[string]any{"recording": rec.ID, "owner": rec.OwnerName})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
