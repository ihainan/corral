package term

import (
	"strconv"
	"strings"
)

// modes follows the terminal modes a program sets in its output, for output
// the ring no longer holds. Programs like herdr, tmux or an editor turn on
// mouse reporting, bracketed paste or the alternate screen once, as they
// start; a window that opens after that output fell out of the ring would
// otherwise replay the screen without them (taps and the wheel reach
// nothing). A full replay starts with them instead (prelude).
//
// Only what the browser's terminal (xterm.js) acts on is kept, as it acts on
// it: one mouse protocol and one encoding at a time, any of 47/1047/1049 for
// the alternate screen, DECSTR and RIS resetting what they reset there. The
// value is plain data, so a copy is a snapshot.
type modes struct {
	// Parser state, carried across writes (a sequence may be split). A CSI
	// is collected as xterm.js does: at most 32 parameters (later ones are
	// dropped), subparameters ignored, one prefix and intermediates.
	st     byte
	c2     bool // the last byte was 0xC2: a C1 control may follow (UTF-8)
	prefix byte // '<', '=', '>' or '?'
	inter  byte // the intermediate byte; 0xff for several
	params [maxParams]int
	np     int  // parameters ended so far (may exceed maxParams)
	cur    int  // the one being read
	sub    bool // reading a subparameter
	begun  bool // a parameter byte or prefix seen
	ignore bool // malformed: the sequence does nothing

	set    uint16 // flags below that differ from a fresh terminal
	mouse  int    // 9, 1000, 1002, 1003; 0 for none
	enc    int    // 1006, 1016; 0 for the default
	keypad bool   // application keypad (DECKPAM, or mode 66)
}

const maxParams = 32

const (
	stGround byte = iota
	stEsc
	stCSI
	stString // OSC, DCS, APC, PM, SOS: skipped up to BEL or ESC
)

// Boolean modes and whether a fresh terminal has them on.
var modeFlags = []struct {
	mode    int
	private bool
	on      bool // the default
	soft    bool // DECSTR resets it
}{
	{1, true, false, true},     // application cursor keys
	{7, true, true, true},      // autowrap
	{25, true, true, true},     // cursor shown
	{1004, true, false, true},  // focus reports
	{2004, true, false, true},  // bracketed paste
	{1049, true, false, false}, // alternate screen (also 47, 1047)
	{4, false, false, true},    // insert mode
}

func (m *modes) startCSI() {
	m.st, m.prefix, m.inter, m.np, m.cur, m.sub, m.begun, m.ignore = stCSI, 0, 0, 0, 0, false, false, false
}

func (m *modes) endParam() {
	if m.np < maxParams {
		m.params[m.np] = m.cur
	}
	m.np++
	m.cur, m.sub = 0, false
}

func (m *modes) feed(p []byte) {
	for _, b := range p {
		// C1 controls arrive UTF-8 encoded (xterm.js decodes before parsing),
		// as 0xC2 then 0x9B etc., and act in any state.
		if m.c2 {
			m.c2 = false
			switch b {
			case 0x9b:
				m.startCSI()
				continue
			case 0x9d, 0x90, 0x9f, 0x9e, 0x98: // OSC, DCS, APC, PM, SOS
				m.st = stString
				continue
			case 0x9c: // ST
				m.st = stGround
				continue
			}
		}
		if b == 0xc2 {
			m.c2 = true
			continue
		}
		switch m.st {
		case stGround:
			if b == 0x1b {
				m.st = stEsc
			}
		case stEsc:
			if b < 0x20 && b != 0x1b && b != 0x18 && b != 0x1a {
				continue // other C0 controls act and leave the sequence going
			}
			m.st = stGround
			switch b {
			case '[':
				m.startCSI()
			case ']', 'P', '_', '^', 'X':
				m.st = stString
			case 0x1b:
				m.st = stEsc
			case 'c': // RIS
				*m = modes{}
			case '=':
				m.keypad = true
			case '>':
				m.keypad = false
			}
		case stCSI:
			switch {
			case b >= '0' && b <= '9':
				m.ignore = m.ignore || m.inter != 0
				m.begun = true
				if !m.sub {
					m.cur = min(m.cur*10+int(b-'0'), 0x7fffffff)
				}
			case b == ';':
				m.ignore = m.ignore || m.inter != 0
				m.begun = true
				m.endParam()
			case b == ':':
				m.ignore = m.ignore || m.inter != 0
				m.begun, m.sub = true, true
			case b >= '<' && b <= '?':
				if m.begun || m.inter != 0 {
					m.ignore = true
				}
				m.prefix, m.begun = b, true
			case b >= 0x20 && b <= 0x2f:
				if m.inter == 0 {
					m.inter = b
				} else {
					m.inter = 0xff
				}
			case b >= 0x40 && b <= 0x7e:
				m.endParam()
				if !m.ignore {
					m.csi(b)
				}
				m.st = stGround
			case b == 0x1b:
				m.st = stEsc
			case b == 0x18 || b == 0x1a: // CAN, SUB
				m.st = stGround
			}
		case stString:
			switch b {
			case 0x07, 0x18, 0x1a:
				m.st = stGround
			case 0x1b: // ST (ESC \) or another sequence, as in xterm.js
				m.st = stEsc
			}
		}
	}
}

func (m *modes) csi(final byte) {
	if final == 'p' && m.inter == '!' && m.prefix == 0 { // DECSTR
		for i, f := range modeFlags {
			if f.soft {
				m.set &^= 1 << i
			}
		}
		m.keypad = false
		return
	}
	if (final != 'h' && final != 'l') || m.inter != 0 || (m.prefix != 0 && m.prefix != '?') {
		return // not SM/RM
	}
	private := m.prefix == '?'
	on := final == 'h'
	for _, n := range m.params[:min(m.np, maxParams)] {
		if private {
			switch n {
			case 9, 1000, 1002, 1003:
				if on {
					m.mouse = n
				} else {
					m.mouse = 0
				}
				continue
			case 1006, 1016:
				if on {
					m.enc = n
				} else {
					m.enc = 0
				}
				continue
			case 66:
				m.keypad = on
				continue
			case 47, 1047:
				n = 1049
			}
		}
		for i, f := range modeFlags {
			if f.mode == n && f.private == private {
				if on != f.on {
					m.set |= 1 << i
				} else {
					m.set &^= 1 << i
				}
			}
		}
	}
}

// prelude is output that puts a fresh terminal in these modes; empty when
// it is in them already.
func (m *modes) prelude() []byte {
	var b strings.Builder
	for i, f := range modeFlags {
		if m.set&(1<<i) == 0 {
			continue
		}
		b.WriteString("\x1b[")
		if f.private {
			b.WriteByte('?')
		}
		b.WriteString(strconv.Itoa(f.mode))
		if f.on {
			b.WriteByte('l')
		} else {
			b.WriteByte('h')
		}
	}
	if m.mouse != 0 {
		b.WriteString("\x1b[?" + strconv.Itoa(m.mouse) + "h")
	}
	if m.enc != 0 {
		b.WriteString("\x1b[?" + strconv.Itoa(m.enc) + "h")
	}
	if m.keypad {
		b.WriteString("\x1b=")
	}
	return []byte(b.String())
}
