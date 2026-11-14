package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
)

// A picture the person sent Claude Code, read again from the log: one whose
// file is not told (sent while it was at work, or pasted into its
// terminal) is only there, as base64 in its message's row, which what is
// read of the log leaves out (b64Strip). Asked for by where it is
// (convo.Part.From), of the conversation as it is read (its log, located by
// the gateway: no path comes from the page); the row is found on the host
// by its uuid (grep, the first line naming it so: a row is written before
// any that hangs on it), checked to be that row, and the picture taken out
// as the parser counts them. Only pictures, of the kinds a page shows;
// the same one asked for at once is read once, and those read lately kept
// (here, not by the browser: see the answer's Cache-Control).

const (
	imageLineMax   = convoLongScan // (a row read for its pictures, at most)
	imageReadLimit = 20 * time.Second
	imageKeptMax   = 48 << 20 // (bytes of pictures kept, at most)
	imageKeptRows  = 256      // (rows kept, at most: one with no picture to give costs little, but not nothing)
	imageRowCost   = 1 << 10  // (what a row kept counts for, besides its pictures)
	imageRowsTried = 4        // (lines naming the uuid looked at, the first that is the row taken)
)

// imageKinds: the kinds of picture given, as their bytes say they are.
var imageKinds = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

type imageKey struct{ sess, log, uuid string }

type imagePics struct {
	pics  [][]byte // (by which one each is: nil, none such)
	kinds []string
	size  int
}

// imageStore keeps the rows' pictures read lately, and the reads going on.
type imageStore struct {
	mu    sync.Mutex
	kept  map[imageKey]*imagePics
	order []imageKey // (oldest first)
	size  int
	going map[imageKey]*imageRead
}

type imageRead struct {
	done chan struct{}
	pics *imagePics
	err  error
}

var images = &imageStore{kept: map[imageKey]*imagePics{}, going: map[imageKey]*imageRead{}}

// of gives the pictures of the row of k, read by read where not kept (one
// read for those asking at once).
func (st *imageStore) of(ctx context.Context, k imageKey, read func(context.Context) (*imagePics, error)) (*imagePics, error) {
	st.mu.Lock()
	if p := st.kept[k]; p != nil {
		st.mu.Unlock()
		return p, nil
	}
	if g := st.going[k]; g != nil {
		st.mu.Unlock()
		select {
		case <-g.done:
			return g.pics, g.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	g := &imageRead{done: make(chan struct{})}
	st.going[k] = g
	st.mu.Unlock()
	// (Not the asker's to cancel: others may wait on it.)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), imageReadLimit)
	g.pics, g.err = read(rctx)
	cancel()
	st.mu.Lock()
	delete(st.going, k)
	if g.err == nil && g.pics.size+imageRowCost <= imageKeptMax {
		st.kept[k] = g.pics
		st.order = append(st.order, k)
		st.size += g.pics.size + imageRowCost
		for (st.size > imageKeptMax || len(st.order) > imageKeptRows) && len(st.order) > 0 {
			old := st.order[0]
			st.order = st.order[1:]
			if p := st.kept[old]; p != nil {
				st.size -= p.size + imageRowCost
				delete(st.kept, old)
			}
		}
	}
	st.mu.Unlock()
	close(g.done)
	return g.pics, g.err
}

// imageRowCommand prints the first lines of log naming uuid as a "uuid"
// (parentUuid and the like are not that; one inside another's object may
// be, so more than one is looked at: rowOf takes the row's own). Its case
// as the log writes it, whatever it is. Fixed command; the validated uuid
// and the located log, quoted.
func imageRowCommand(log, uuid string) string {
	return "grep -m " + strconv.Itoa(imageRowsTried) + " -i -F -e " + sshx.QuoteShell(`"uuid":"`+uuid+`"`) + " -- " + sshx.QuoteShell(log)
}

// rowsPictures: of the lines grep printed, the pictures of the first that
// is the row of uuid (convo.ErrNoImage where none is).
func rowsPictures(out []byte, uuid string) (*imagePics, error) {
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if p, err := picturesOf(line, uuid); err == nil {
			return p, nil
		}
	}
	return nil, convo.ErrNoImage
}

// picturesOf takes the pictures out of a row: only those whose bytes are
// of a kind given (imageKinds), and are what the row says they are.
func picturesOf(line []byte, uuid string) (*imagePics, error) {
	ims, err := convo.ImagesOf(line, uuid)
	if err != nil {
		return nil, err
	}
	p := &imagePics{}
	for _, im := range ims {
		data, sniffed := im.Data, ""
		if data != nil {
			sniffed = http.DetectContentType(data)
		}
		if !imageKinds[sniffed] || (im.Media != "" && !strings.EqualFold(im.Media, sniffed)) {
			data, sniffed = nil, ""
		}
		p.pics, p.kinds = append(p.pics, data), append(p.kinds, sniffed)
		p.size += len(data)
	}
	return p, nil
}

// apiConversationImage: GET …/conversation/image?pane=<pane>&from=<uuid>.<n>
func (s *Server) apiConversationImage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pane := q.Get("pane")
	uuid, n, good := convo.ImageAt(q.Get("from"))
	if (pane != "" && !herdrPane.MatchString(pane)) || !good {
		writeError(w, http.StatusBadRequest, "bad_request", "bad pane or picture")
		return
	}
	key := sess.ID + "\x00"
	if sess.Persistence == "herdr" {
		key += pane
	}
	main := s.convos.peek(key)
	if main == nil {
		writeError(w, http.StatusConflict, "not_located", "the conversation is not read yet")
		return
	}
	main.mu.Lock()
	log, agentKind, reason := main.log, main.agent, main.reason
	main.mu.Unlock()
	if log == "" || reason != "" || agentKind != "claude" || main.sub || !strings.HasSuffix(log, ".jsonl") {
		writeError(w, http.StatusConflict, "not_located", "the conversation is not read yet")
		return
	}
	pics, err := images.of(r.Context(), imageKey{sess.ID, log, uuid}, func(ctx context.Context) (*imagePics, error) {
		out, err := sshx.Output(ctx, sess.Client(), imageRowCommand(log, uuid), imageLineMax)
		if err != nil {
			var exit *sshx.ExitError
			if errors.As(err, &exit) && exit.Status == 1 {
				return nil, convo.ErrNoImage // (grep found none)
			}
			return nil, err
		}
		return rowsPictures(out, uuid)
	})
	if err != nil {
		if errors.Is(err, convo.ErrNoImage) {
			writeError(w, http.StatusNotFound, "not_found", "no such picture")
			return
		}
		if r.Context().Err() != nil {
			return
		}
		writeError(w, http.StatusBadGateway, "read_failed", "the log could not be read for it")
		return
	}
	if n >= len(pics.pics) || pics.pics[n] == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such picture")
		return
	}
	h := w.Header()
	h.Set("Content-Type", pics.kinds[n])
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// (Not kept by the browser: one signed in after on it would be shown
	// it without asking. Asked again, it is kept here.)
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(pics.pics[n])
}
