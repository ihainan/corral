package term

// Alert scanning: terminal programs signal "look at me" with a bell (BEL) or a
// notification escape (OSC 9 / OSC 777;notify). The gateway watches the output
// stream for them, because a phone tab that is in the background cannot.

import "unicode/utf8"

type Alert struct {
	Message string // empty for a plain bell
}

// alertScanner is fed output chunks in order and remembers state across them,
// so a BEL that terminates an OSC sequence is not mistaken for a bell.
type alertScanner struct {
	state int // 0 normal, 1 after ESC, 2 inside OSC, 3 ESC inside OSC
	osc   []byte
}

const maxOSC = 1024

func (a *alertScanner) feed(p []byte, emit func(Alert)) {
	for _, b := range p {
		switch a.state {
		case 0:
			switch b {
			case 0x07:
				emit(Alert{})
			case 0x1b:
				a.state = 1
			}
		case 1:
			if b == ']' {
				a.state, a.osc = 2, a.osc[:0]
			} else {
				a.state = 0
			}
		case 2:
			switch b {
			case 0x07:
				a.finish(emit)
			case 0x1b:
				a.state = 3
			default:
				if len(a.osc) < maxOSC {
					a.osc = append(a.osc, b)
				}
			}
		case 3:
			if b == '\\' {
				a.finish(emit)
			} else {
				a.state = 2
			}
		}
	}
}

func (a *alertScanner) finish(emit func(Alert)) {
	a.state = 0
	s := string(a.osc)
	switch {
	case len(s) > 2 && s[:2] == "9;":
		msg := s[2:]
		// OSC 9;4;… is ConEmu's progress report, not a notification.
		if len(msg) > 1 && msg[0] == '4' && msg[1] == ';' {
			return
		}
		emit(Alert{Message: clean(msg)})
	case len(s) > 11 && s[:11] == "777;notify;":
		rest := s[11:]
		// title;body
		for i := 0; i < len(rest); i++ {
			if rest[i] == ';' {
				title, body := rest[:i], rest[i+1:]
				if body == "" {
					emit(Alert{Message: clean(title)})
				} else {
					emit(Alert{Message: clean(title + ": " + body)})
				}
				return
			}
		}
		emit(Alert{Message: clean(rest)})
	}
}

// clean keeps notification text printable and short.
func clean(s string) string {
	if !utf8.ValidString(s) {
		return ""
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		out = append(out, r)
		if len(out) >= 200 {
			break
		}
	}
	return string(out)
}
