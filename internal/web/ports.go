package web

import (
	"bufio"
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

// listenCommand lists listening TCP sockets. Fixed text; `ss` is preferred,
// /proc/net/tcp is the fallback on minimal hosts.
const listenCommand = `sh -c 'ss -ltnH 2>/dev/null || { echo "#proc"; cat /proc/net/tcp /proc/net/tcp6 2>/dev/null; }'`

type ListeningPort struct {
	Port    int    `json:"port"`
	Address string `json:"address"` // what it is bound to: 127.0.0.1, 0.0.0.0, ::
}

// listeningPorts runs listenCommand over an existing SSH connection.
func listeningPorts(ctx context.Context, c *ssh.Client) ([]ListeningPort, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := sshx.Output(ctx, c, listenCommand, sshx.MaxOutput)
	switch {
	case ctx.Err() != nil:
		return nil, errors.New("listing ports timed out")
	case errors.Is(err, sshx.ErrOutputTooLarge):
		return nil, err
	case err != nil && len(out) == 0:
		return nil, err
	}
	return parseListening(string(out)), nil
}

func parseListening(out string) []ListeningPort {
	seen := map[int]ListeningPort{}
	sc := bufio.NewScanner(strings.NewReader(out))
	proc := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "#proc" {
			proc = true
			continue
		}
		f := strings.Fields(line)
		if proc {
			// sl local_address rem_address st ...; state 0A is LISTEN; address is hex.
			if len(f) < 4 || f[3] != "0A" {
				continue
			}
			addr, port, ok := parseProcAddr(f[1])
			if ok {
				add(seen, port, addr)
			}
			continue
		}
		// ss -ltnH: State Recv-Q Send-Q Local-Address:Port Peer-Address:Port
		if len(f) < 4 {
			continue
		}
		local := f[3]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		addr := strings.Trim(local[:i], "[]")
		if j := strings.Index(addr, "%"); j >= 0 {
			addr = addr[:j]
		}
		add(seen, port, addr)
	}
	out2 := make([]ListeningPort, 0, len(seen))
	for _, p := range seen {
		out2 = append(out2, p)
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].Port < out2[j].Port })
	return out2
}

// add keeps one entry per port, ignoring well-known system ports and ports
// only reachable on another interface.
func add(seen map[int]ListeningPort, port int, addr string) {
	if port < 1024 || port > 65535 {
		return
	}
	switch addr {
	case "127.0.0.1", "0.0.0.0", "::1", "::", "*", "localhost":
	default:
		if !strings.HasPrefix(addr, "127.") {
			return // bound to a specific external address; 127.0.0.1 cannot reach it
		}
	}
	if _, ok := seen[port]; !ok {
		seen[port] = ListeningPort{Port: port, Address: addr}
	}
}

func parseProcAddr(s string) (string, int, bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", 0, false
	}
	port, err := strconv.ParseUint(s[i+1:], 16, 16)
	if err != nil {
		return "", 0, false
	}
	hex := s[:i]
	switch hex {
	case "00000000", "00000000000000000000000000000000":
		return "0.0.0.0", int(port), true
	case "0100007F":
		return "127.0.0.1", int(port), true
	case "00000000000000000000000001000000":
		return "::1", int(port), true
	}
	if len(hex) == 8 && strings.HasSuffix(hex, "7F") {
		return "127.0.0.x", int(port), true
	}
	return hex, int(port), true
}
