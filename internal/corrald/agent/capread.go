//go:build linux

// Package agent implements the host-side corrald agent locator, safe log tail
// reader, allowlisted tmux metadata executor, and inotify/proc change watcher.
package agent

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// secretEnvCaps lists declared env capabilities whose values must NEVER be
// extracted into Go strings or copied; callers may only test non-empty presence
// via readProcEnvHasNonEmpty.
var secretEnvCaps = map[string]bool{
	proto.ReadEnvAnthropicAuthToken: true,
	proto.ReadEnvAnthropicAPIKey:    true,
}

// capReader enforces that every host read declares a capability category
// present in proto.DefaultManifest().Reads.
type capReader struct {
	procRoot string
}

func newCapReader(procRoot string) *capReader {
	if procRoot == "" {
		procRoot = "/proc"
	}
	return &capReader{procRoot: procRoot}
}

func assertDeclaredRead(cap string) {
	if !proto.IsDeclaredRead(cap) {
		panic(fmt.Sprintf("corrald/agent: undeclared read capability %q", cap))
	}
}

func (r *capReader) readFile(cap, path string) ([]byte, error) {
	assertDeclaredRead(cap)
	return os.ReadFile(path)
}

func (r *capReader) readFileLimit(cap, path string, maxBytes int64) ([]byte, error) {
	assertDeclaredRead(cap)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

func (r *capReader) readTail(cap, path string, maxBytes int64) ([]byte, error) {
	assertDeclaredRead(cap)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readTailFromFile(f, maxBytes)
}

// readTailFromFile reads at most maxBytes from the end of f, bounding memory
// allocation strictly with io.LimitReader even if f is actively growing.
func readTailFromFile(f *os.File, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, nil
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size > maxBytes {
		if _, err := f.Seek(size-maxBytes, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

func (r *capReader) stat(cap, path string) (os.FileInfo, error) {
	assertDeclaredRead(cap)
	return os.Stat(path)
}

func (r *capReader) isRegularFile(cap, path string) bool {
	info, err := r.stat(cap, path)
	return err == nil && info.Mode().IsRegular()
}

func (r *capReader) isDir(cap, path string) bool {
	info, err := r.stat(cap, path)
	return err == nil && info.IsDir()
}

func (r *capReader) isReadableFile(cap, path string) bool {
	assertDeclaredRead(cap)
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func (r *capReader) ownedByUID(cap, path string, uid uint32) bool {
	info, err := r.stat(cap, path)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid
}

func (r *capReader) readLink(cap, path string) (string, error) {
	assertDeclaredRead(cap)
	return os.Readlink(path)
}

func (r *capReader) evalSymlinks(cap, path string) (string, error) {
	assertDeclaredRead(cap)
	return filepath.EvalSymlinks(path)
}

func (r *capReader) readDirNames(cap, dir string) ([]string, error) {
	assertDeclaredRead(cap)
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// envKV is a single NUL-delimited non-secret environment entry extracted from
// /proc/<pid>/environ with embedded newlines normalized to \x01 so a value can
// never forge another variable.
type envKV struct {
	key string
	val string
}

// readProcEnv reads only the non-secret environment variables whose
// proto.ReadEnv* capability constants are passed in envCaps. Secret env caps
// (such as API keys / auth tokens) are rejected here and must be checked via
// readProcEnvHasNonEmpty so their values are never materialized as Go strings.
func (r *capReader) readProcEnv(pidDir string, envCaps ...string) ([]envKV, error) {
	assertDeclaredRead(proto.ReadProc)
	allowed := make(map[string]struct{}, len(envCaps))
	for _, c := range envCaps {
		assertDeclaredRead(c)
		if secretEnvCaps[c] {
			panic(fmt.Sprintf("corrald/agent: secret env capability %q must use readProcEnvHasNonEmpty", c))
		}
		name, ok := strings.CutPrefix(c, "env:")
		if !ok || name == "" {
			panic(fmt.Sprintf("corrald/agent: non-env capability passed to readProcEnv: %q", c))
		}
		allowed[name] = struct{}{}
	}
	raw, err := os.ReadFile(filepath.Join(pidDir, "environ"))
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	return filterEnvironBytes(raw, allowed), nil
}

// readProcEnvHasNonEmpty checks whether the first matching entry for envCap in
// <pidDir>/environ has a non-empty value, without ever allocating a Go string
// containing the value and zeroing the temporary environ buffer before return.
func (r *capReader) readProcEnvHasNonEmpty(pidDir, envCap string) bool {
	assertDeclaredRead(proto.ReadProc)
	assertDeclaredRead(envCap)
	name, ok := strings.CutPrefix(envCap, "env:")
	if !ok || name == "" {
		panic(fmt.Sprintf("corrald/agent: non-env capability passed to readProcEnvHasNonEmpty: %q", envCap))
	}
	raw, err := os.ReadFile(filepath.Join(pidDir, "environ"))
	if err != nil {
		return false
	}
	defer clear(raw)
	prefix := append([]byte(name), '=')
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if !bytes.HasPrefix(entry, prefix) {
			continue
		}
		val := entry[len(prefix):]
		return len(val) > 0
	}
	return false
}

// readSelfEnv reads a single declared non-secret environment variable from
// /proc/self/environ without calling os.Getenv.
func (r *capReader) readSelfEnv(envCap string) string {
	kvs, err := r.readProcEnv(filepath.Join(r.procRoot, "self"), envCap)
	if err != nil || len(kvs) == 0 {
		return ""
	}
	return kvs[0].val
}

// filterEnvironBytes parses NUL-separated environ bytes and returns only entries
// whose key is in allowed, preserving original order and translating embedded
// '\n' bytes in values to '\x01' (matching `tr '\0\n' '\n\001'`).
func filterEnvironBytes(raw []byte, allowed map[string]struct{}) []envKV {
	if len(raw) == 0 {
		return nil
	}
	var out []envKV
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		eq := bytes.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		key := string(entry[:eq])
		if _, ok := allowed[key]; !ok {
			continue
		}
		valBytes := entry[eq+1:]
		if bytes.IndexByte(valBytes, '\n') >= 0 {
			valBytes = bytes.ReplaceAll(valBytes, []byte{'\n'}, []byte{0x01})
		}
		out = append(out, envKV{key: key, val: string(valBytes)})
	}
	return out
}

func firstEnvVal(kvs []envKV, key string) string {
	for _, kv := range kvs {
		if kv.key == key {
			return kv.val
		}
	}
	return ""
}
