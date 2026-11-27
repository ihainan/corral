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
	"sort"
	"strings"
	"syscall"

	"github.com/ihainan/corral/internal/corrald/proto"
)

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

func readTailFromFile(f *os.File, maxBytes int64) ([]byte, error) {
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
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		data = data[int64(len(data))-maxBytes:]
	}
	return data, nil
}

func (r *capReader) stat(cap, path string) (os.FileInfo, error) {
	assertDeclaredRead(cap)
	return os.Stat(path)
}

func (r *capReader) lstat(cap, path string) (os.FileInfo, error) {
	assertDeclaredRead(cap)
	return os.Lstat(path)
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

func (r *capReader) glob(cap, pattern string) ([]string, error) {
	assertDeclaredRead(cap)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
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

// envKV is a single NUL-delimited environment entry extracted from
// /proc/<pid>/environ with embedded newlines normalized to \x01 so a value can
// never forge another variable.
type envKV struct {
	key string
	val string
}

// readProcEnv reads only the environment variables whose proto.ReadEnv*
// capability constants are passed in envCaps. Every passed constant must be in
// proto.DefaultManifest().Reads and start with "env:".
func (r *capReader) readProcEnv(pidDir string, envCaps ...string) ([]envKV, error) {
	assertDeclaredRead(proto.ReadProc)
	allowed := make(map[string]struct{}, len(envCaps))
	for _, c := range envCaps {
		assertDeclaredRead(c)
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
	return filterEnvironBytes(raw, allowed), nil
}

// readSelfEnv reads a single declared environment variable from
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
