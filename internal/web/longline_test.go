package web

import (
	"bytes"
	"strings"
	"testing"
)

type memFile []byte

func (m memFile) ReadRange(_ string, off int64, n int) ([]byte, error) {
	if off >= int64(len(m)) {
		return nil, nil
	}
	return m[off:min(int64(len(m)), off+int64(n))], nil
}

// A line longer than an older part (a message with images) is read whole
// from where it begins, its images' data left out; one longer than
// convoLongScan is passed over.
func TestLongLineBefore(t *testing.T) {
	img := strings.Repeat("QUFB", (6<<20)/4) // (6 MB of base64)
	big := `{"type":"user","message":{"content":[{"type":"text","text":"look at this"},{"type":"image","source":{"data":"` + img + `"}}]}}`
	head := `{"type":"user","message":{"content":"before"}}` + "\n"
	log := memFile(head + big + "\n" + `{"type":"assistant"}` + "\n")
	end := int64(len(head) + len(big) + 1) // (where the line after it begins: what was read)
	start := end - convoOlderStep
	line, from, err := longLineBefore(log, "x", start, log[start:end])
	if err != nil {
		t.Fatal(err)
	}
	if from != int64(len(head)) {
		t.Fatalf("from %d, want where it begins, %d", from, len(head))
	}
	if !bytes.HasSuffix(line, []byte("\n")) || !bytes.Contains(line, []byte(`"text":"look at this"`)) || len(line) > 4096 {
		t.Fatalf("line %d bytes: %.200q", len(line), line)
	}
	// Its start at the file's: from 0.
	log2 := memFile(big + "\n")
	end = int64(len(log2))
	line, from, err = longLineBefore(log2, "x", end-convoOlderStep, log2[end-convoOlderStep:])
	if err != nil || from != 0 || !bytes.Contains(line, []byte("look at this")) {
		t.Fatalf("at the start: from %d, err %v, %.100q", from, err, line)
	}
	// Longer than convoLongScan: passed over, from inside it.
	huge := memFile(head + `{"data":"` + strings.Repeat("A", convoLongScan+(1<<20)) + `"}` + "\n")
	end = int64(len(huge))
	line, from, err = longLineBefore(huge, "x", end-convoOlderStep, huge[end-convoOlderStep:])
	if err != nil || line != nil || from <= int64(len(head)) {
		t.Fatalf("too long: line %d bytes, from %d, err %v", len(line), from, err)
	}
	// Too long even with the images' data left out: passed over.
	text := memFile(head + `{"text":"` + strings.Repeat("x ", 3<<20) + `"}` + "\n")
	end = int64(len(text))
	line, _, err = longLineBefore(text, "x", end-convoOlderStep, text[end-convoOlderStep:])
	if err != nil || line != nil {
		t.Fatalf("text too long: line %d bytes, err %v", len(line), err)
	}
}
