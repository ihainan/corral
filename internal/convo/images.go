package convo

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Pictures the person sent are in Claude Code's log as base64, in the row
// of their message (its content, or the prompt of a message sent while it
// was at work: an attachment). What is read of the log leaves that out
// (web's b64Strip); for one whose file is not told — sent while Claude Code
// was at work, or pasted into its terminal — the row is read again for it,
// asked for by where it is (Part.From).

const maxImageRow = 64 // (pictures of one row, by which one each is)

var (
	rowUUID     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	imageFromRe = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.([0-9]{1,2})$`)
)

// imageFrom: where the n-th picture of the row of uuid is ("" where that
// cannot be told).
func imageFrom(uuid string, n int) string {
	if !rowUUID.MatchString(uuid) || n < 0 || n >= maxImageRow {
		return ""
	}
	return strings.ToLower(uuid) + "." + strconv.Itoa(n)
}

// ImageAt reads a Part.From: the row's uuid (lower case) and which of its
// pictures; ok false where it is not one.
func ImageAt(from string) (uuid string, n int, ok bool) {
	m := imageFromRe.FindStringSubmatch(from)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n >= maxImageRow {
		return "", 0, false
	}
	return strings.ToLower(m[1]), n, true
}

var (
	ErrNoImage = errors.New("no such picture in the row")
	errNotRow  = errors.New("not the row asked for")
)

// Image is one of a row's pictures: its kind as the row says it, its
// bytes (nil where it is not one to be had: not base64, or not decoded).
type Image struct {
	Media string
	Data  []byte
}

// ImagesOf takes out of line, a row of Claude Code's log, its pictures, in
// order, counted as the parser counts them (Part.From): the blocks of type
// image of its message's content, or of its attachment's prompt (at most
// maxImageRow). The row must be the one of uuid.
func ImagesOf(line []byte, uuid string) ([]Image, error) {
	var r struct {
		UUID    string `json:"uuid"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Attachment struct {
			Prompt json.RawMessage `json:"prompt"`
		} `json:"attachment"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, err
	}
	if !strings.EqualFold(r.UUID, uuid) {
		return nil, errNotRow
	}
	type block struct {
		Type   string `json:"type"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	var blocks []block
	if json.Unmarshal(r.Message.Content, &blocks) != nil || len(blocks) == 0 {
		blocks = nil
		_ = json.Unmarshal(r.Attachment.Prompt, &blocks)
	}
	var out []Image
	for _, b := range blocks {
		if b.Type != "image" {
			continue
		}
		if len(out) >= maxImageRow {
			break
		}
		im := Image{Media: b.Source.MediaType}
		if b.Source.Type == "base64" && b.Source.Data != "" {
			if d, err := base64.StdEncoding.DecodeString(b.Source.Data); err == nil {
				im.Data = d
			}
		}
		out = append(out, im)
	}
	return out, nil
}

// ImageOf is the n-th of ImagesOf (ErrNoImage where there is none such).
func ImageOf(line []byte, uuid string, n int) (media string, data []byte, err error) {
	ims, err := ImagesOf(line, uuid)
	if err != nil {
		return "", nil, err
	}
	if n < 0 || n >= len(ims) || ims[n].Data == nil {
		return "", nil, ErrNoImage
	}
	return ims[n].Media, ims[n].Data, nil
}
