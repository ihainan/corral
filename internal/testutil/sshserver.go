// Package testutil provides an in-process SSH server for tests: password auth,
// a PTY "shell" that echoes input back, and an SFTP subsystem rooted in a temp dir.
package testutil

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type SSHServer struct {
	Addr         *net.TCPAddr
	HostKey      ssh.PublicKey
	ln           net.Listener
	mu           sync.Mutex
	conns        []net.Conn
	Execs        []string
	cfg          *ssh.ServerConfig
	user, pass   string
	AuthAttempts int // password checks received, to prove credentials were (not) sent
	onExec       func(cmd string) func(ch ssh.Channel)
	onChannel    func(nc ssh.NewChannel) bool
	beforeAuth   func()
}

// BeforeAuth runs f in every password check before answering it, e.g. to hold
// a client mid-connect.
func (s *SSHServer) BeforeAuth(f func()) {
	s.mu.Lock()
	s.beforeAuth = f
	s.mu.Unlock()
}

// HandleExec lets a test run a command itself: f returns what to do with the
// channel, or nil for the default echoing shell. The channel closes after.
// Commands are the exec requests received so far.
func (s *SSHServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Execs...)
}

func (s *SSHServer) HandleExec(f func(cmd string) func(ch ssh.Channel)) {
	s.mu.Lock()
	s.onExec = f
	s.mu.Unlock()
}

// HandleChannel lets a test take channels of other types (e.g. Unix socket
// forwarding); returning false falls back to the default handling.
func (s *SSHServer) HandleChannel(f func(nc ssh.NewChannel) bool) {
	s.mu.Lock()
	s.onChannel = f
	s.mu.Unlock()
}

// StartSSHServer accepts user/password and serves until the test ends.
func StartSSHServer(t *testing.T, user, password, sftpRoot string) *SSHServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &SSHServer{Addr: ln.Addr().(*net.TCPAddr), ln: ln, user: user, pass: password}
	s.RotateHostKey()
	t.Cleanup(s.Close)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			s.mu.Lock()
			cfg := s.cfg
			s.mu.Unlock()
			go s.serve(c, cfg, sftpRoot)
		}
	}()
	return s
}

// RotateHostKey makes the server present a new host key to later connections.
func (s *SSHServer) RotateHostKey() {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		panic(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			s.mu.Lock()
			s.AuthAttempts++
			beforeAuth := s.beforeAuth
			s.mu.Unlock()
			if beforeAuth != nil {
				beforeAuth()
			}
			if c.User() == s.user && string(pw) == s.pass {
				return nil, nil
			}
			return nil, errAuth
		},
	}
	cfg.AddHostKey(signer)
	s.mu.Lock()
	s.cfg = cfg
	s.HostKey = signer.PublicKey()
	s.mu.Unlock()
}

func (s *SSHServer) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AuthAttempts
}

type authErr struct{}

func (authErr) Error() string { return "denied" }

var errAuth = authErr{}

func (s *SSHServer) Close() {
	s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
}

func (s *SSHServer) serve(c net.Conn, cfg *ssh.ServerConfig, root string) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer sc.Close()
	go func() {
		for r := range reqs {
			if r.WantReply {
				r.Reply(r.Type == "keepalive@openssh.com", nil)
			}
		}
	}()
	for nc := range chans {
		s.mu.Lock()
		onChannel := s.onChannel
		s.mu.Unlock()
		if onChannel != nil && onChannel(nc) {
			continue
		}
		if nc.ChannelType() == "direct-streamlocal@openssh.com" {
			go directStreamLocal(nc)
			continue
		}
		if nc.ChannelType() == "direct-tcpip" {
			go directTCPIP(nc)
			continue
		}
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.session(ch, creqs, root)
	}
}

func (s *SSHServer) session(ch ssh.Channel, reqs <-chan *ssh.Request, root string) {
	defer ch.Close()
	for r := range reqs {
		switch r.Type {
		case "pty-req", "env", "window-change":
			if r.WantReply {
				r.Reply(true, nil)
			}
		case "shell", "exec":
			if r.Type == "exec" && len(r.Payload) >= 4 {
				n := binary.BigEndian.Uint32(r.Payload)
				if int(n) <= len(r.Payload)-4 {
					cmd := string(r.Payload[4 : 4+n])
					s.mu.Lock()
					s.Execs = append(s.Execs, cmd)
					onExec := s.onExec
					s.mu.Unlock()
					if onExec != nil {
						if run := onExec(cmd); run != nil {
							r.Reply(true, nil)
							go ssh.DiscardRequests(reqs)
							run(ch)
							return
						}
					}
				}
			}
			r.Reply(true, nil)
			ch.Write([]byte("ready\r\n"))
			go func() {
				// Echo input. Typing "burst!" makes the "program" print 300 KB and exit
				// at once, to test that no output is lost when a session ends.
				buf := make([]byte, 4096)
				var line []byte
				for {
					n, err := ch.Read(buf)
					if n > 0 {
						ch.Write(buf[:n])
						line = append(line, buf[:n]...)
						if bytes.Contains(line, []byte("burst!")) {
							chunk := bytes.Repeat([]byte("0123456789abcdef"), 64)
							for i := 0; i < 300; i++ {
								ch.Write(chunk)
							}
							ch.Write([]byte("END-OF-BURST"))
							break
						}
						if len(line) > 1024 {
							line = line[len(line)-64:]
						}
					}
					if err != nil {
						break
					}
				}
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
				ch.Close()
			}()
		case "subsystem":
			if len(r.Payload) >= 4 && string(r.Payload[4:]) == "sftp" {
				r.Reply(true, nil)
				srv, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(root))
				if err != nil {
					return
				}
				srv.Serve()
				return
			}
			r.Reply(false, nil)
		default:
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}
}

// directTCPIP serves "ssh -L"-style channels by dialing the requested address.
func directTCPIP(nc ssh.NewChannel) {
	var req struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil {
		nc.Reject(ssh.ConnectionFailed, "bad request")
		return
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port))))
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { io.Copy(ch, conn); ch.CloseWrite() }()
	io.Copy(conn, ch)
	conn.Close()
	ch.Close()
}

// directStreamLocal serves "ssh -L"-style channels to a Unix socket.
func directStreamLocal(nc ssh.NewChannel) {
	var req struct {
		SocketPath string
		Reserved0  string
		Reserved1  uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil {
		nc.Reject(ssh.ConnectionFailed, "bad request")
		return
	}
	conn, err := net.Dial("unix", req.SocketPath)
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { io.Copy(ch, conn); ch.CloseWrite() }()
	io.Copy(conn, ch)
	conn.Close()
	ch.Close()
}

// ShellExec runs exec requests that match through the local sh, with HOME set
// to home and extra environment entries, like a real host would. Others fall
// back to the default echoing shell.
func ShellExec(home string, match func(cmd string) bool, env ...string) func(cmd string) func(ssh.Channel) {
	return func(cmd string) func(ssh.Channel) {
		if !match(cmd) {
			return nil
		}
		return func(ch ssh.Channel) {
			c := exec.Command("sh", "-c", cmd)
			c.Dir = home
			c.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
			c.Stdout = ch
			c.Stderr = ch.Stderr()
			// The channel's input goes to the command, as with sshd; the copy is
			// not waited for, so a client that never closes its input cannot hold
			// up the command's end.
			if in, err := c.StdinPipe(); err == nil {
				go func() {
					io.Copy(in, ch)
					in.Close()
				}()
			}
			status := byte(0)
			if err := c.Run(); err != nil {
				status = 1
				var ee *exec.ExitError
				if errors.As(err, &ee) && ee.ExitCode() > 0 {
					status = byte(ee.ExitCode())
				}
			}
			ch.CloseWrite()
			ch.SendRequest("exit-status", false, []byte{0, 0, 0, status})
		}
	}
}
