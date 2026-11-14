package web

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const imgUUID = "aaaaaaaa-2222-3333-4444-555555555555"

func imgRow(uuid string, blocks ...string) string {
	return `{"type":"attachment","uuid":"` + uuid + `","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"[Image #1] 看"},` + strings.Join(blocks, ",") + `]}}`
}

func imgBlock(media string, data []byte) string {
	return `{"type":"image","source":{"type":"base64","media_type":"` + media + `","data":"` + base64.StdEncoding.EncodeToString(data) + `"}}`
}

var (
	pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpgBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
)

// Only pictures of a kind a page shows, and what the row says they are:
// one whose bytes are another thing (a page, a script) is not given, nor
// one said to be a PNG that is a JPEG. Each keeps its place.
func TestPicturesOf(t *testing.T) {
	line := imgRow(imgUUID,
		imgBlock("image/png", pngBytes),
		imgBlock("image/png", []byte("<html><script>alert(1)</script></html>")),
		imgBlock("image/png", jpgBytes),
		imgBlock("image/jpeg", jpgBytes),
		`{"type":"image","source":{"type":"url","url":"https://x/y.png"}}`,
	)
	p, err := picturesOf([]byte(line), imgUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.pics) != 5 {
		t.Fatalf("pictures: %d", len(p.pics))
	}
	if p.kinds[0] != "image/png" || p.pics[0] == nil || p.kinds[3] != "image/jpeg" || p.pics[3] == nil {
		t.Errorf("good ones: %q", p.kinds)
	}
	for _, i := range []int{1, 2, 4} {
		if p.pics[i] != nil || p.kinds[i] != "" {
			t.Errorf("%d given: %q", i, p.kinds[i])
		}
	}
	if p.size != len(pngBytes)+len(jpgBytes) {
		t.Errorf("size %d", p.size)
	}
	if _, err := picturesOf([]byte(line), "bbbbbbbb-2222-3333-4444-555555555555"); err == nil {
		t.Error("another row taken for it")
	}
}

// The command finds the row by its own uuid: not one that hangs on it
// (parentUuid), nor one that quotes it in its words, though either comes
// first; a uuid and a path with quotes in them stay one argument each.
func TestImageRowCommand(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "it's a log.jsonl")
	rows := []string{
		`{"type":"user","uuid":"11111111-2222-3333-4444-555555555555","parentUuid":"` + imgUUID + `"}`,
		`{"type":"user","uuid":"22222222-2222-3333-4444-555555555555","message":{"content":"see \"uuid\":\"` + imgUUID + `\""}}`,
		imgRow(imgUUID, imgBlock("image/png", pngBytes)),
		imgRow(imgUUID, imgBlock("image/jpeg", jpgBytes)),
	}
	if err := os.WriteFile(log, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", imageRowCommand(log, imgUUID)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != rows[2] {
		t.Fatalf("found: %.120s", out)
	}
	p, err := picturesOf(out, imgUUID)
	if err != nil || len(p.pics) != 1 || p.kinds[0] != "image/png" {
		t.Errorf("%v %+v", err, p)
	}
	// (None such: grep's status 1.)
	err = exec.Command("sh", "-c", imageRowCommand(log, "cccccccc-2222-3333-4444-555555555555")).Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Errorf("none: %v", err)
	}
}

// Asked for at once, a row is read once; read, it is kept, the oldest let
// go past the bound; a read that failed is not kept.
func TestImageStore(t *testing.T) {
	st := &imageStore{kept: map[imageKey]*imagePics{}, going: map[imageKey]*imageRead{}}
	var reads atomic.Int32
	gate := make(chan struct{})
	read := func(size int) func(context.Context) (*imagePics, error) {
		return func(context.Context) (*imagePics, error) {
			reads.Add(1)
			<-gate
			return &imagePics{pics: [][]byte{make([]byte, size)}, kinds: []string{"image/png"}, size: size}, nil
		}
	}
	k1 := imageKey{"s", "/l.jsonl", "a"}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := st.of(context.Background(), k1, read(10)); err != nil || p.size != 10 {
				t.Errorf("%v %+v", err, p)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := reads.Load(); n != 1 {
		t.Errorf("reads at once: %d", n)
	}
	if _, err := st.of(context.Background(), k1, read(10)); err != nil || reads.Load() != 1 {
		t.Errorf("kept: %v %d", err, reads.Load())
	}
	// (Past the bound: the oldest let go.)
	big := imageKeptMax - 5
	if _, err := st.of(context.Background(), imageKey{"s", "/l.jsonl", "b"}, read(big)); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	_, still := st.kept[k1]
	size := st.size
	st.mu.Unlock()
	if still || size != big {
		t.Errorf("still kept %v, size %d", still, size)
	}
	// (Failed: not kept, read again next time.)
	k3 := imageKey{"s", "/l.jsonl", "c"}
	_, err := st.of(context.Background(), k3, func(context.Context) (*imagePics, error) { return nil, context.DeadlineExceeded })
	if err == nil {
		t.Fatal("no error")
	}
	if _, err := st.of(context.Background(), k3, read(1)); err != nil {
		t.Errorf("again: %v", err)
	}
}
