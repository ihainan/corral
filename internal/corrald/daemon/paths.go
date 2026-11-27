// Package daemon implements the host-side corrald binary: path layout,
// single-instance locking, Unix domain socket RPC server, event bus,
// idempotent write table, hot-update handoff, and CLI subcommands.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// MaxSocketPath is the maximum Unix domain socket path length allowed before
// falling back to a private per-user directory under /tmp.
const MaxSocketPath = 99

var (
	hex12RE       = regexp.MustCompile(`^[0-9a-f]{12}$`)
	versionPartRE = regexp.MustCompile(`^[0-9A-Za-z._+-]{1,128}$`)
	digitsRE      = regexp.MustCompile(`^[0-9]+$`)
)

// MachineIDSource names where the host's machine identifier was obtained.
const (
	MachineIDSourceEtc      = "/etc/machine-id"
	MachineIDSourceDbus     = "/var/lib/dbus/machine-id"
	MachineIDSourceHostname = "hostname"
	MachineIDSourceOverride = "override"
)

// Layout holds all host-side paths for a given (home, uid, machineID) tuple.
type Layout struct {
	Home            string
	UID             uint32
	MachineID       string
	MachineIDSource string

	LibDir         string // ~/.local/lib/corral
	CurrentSymlink string // ~/.local/lib/corral/corrald
	ReadmePath     string // ~/.local/lib/corral/README.txt
	SharedLockPath string // ~/.local/lib/corral/.corrald-shared.lock

	StateRoot     string // ~/.local/state/corral/corrald
	StateDir      string // ~/.local/state/corral/corrald/<mid>
	InstallJSON   string // .../<mid>/install.json
	LockPath      string // .../<mid>/corrald.lock
	OpLockPath    string // .../<mid>/op.lock
	MetaJSON      string // .../<mid>/meta.json
	ClientsJSON   string // .../<mid>/clients.json
	LogPath       string // .../<mid>/corrald.log
	OpsDir        string // .../<mid>/ops
	TombstonePath string // .../<mid>/removing
	FailedJSON    string // .../<mid>/failed.json

	SocketPath       string // .../<mid>/corrald.sock or /tmp/corrald-<uid>-<hash>/corrald.sock
	SocketDir        string // parent directory of SocketPath
	SocketUsesTmpDir bool   // true when SocketPath fell back to /tmp
}

// PathOptions configures Layout resolution (primarily for unit tests).
type PathOptions struct {
	Home            string
	UID             *uint32
	MachineID       string
	MachineIDSource string
	TmpBase         string // defaults to "/tmp"
	MachineIDPaths  []string
	HostnameFn      func() (string, error)
}

// ResolveMachineID determines the 12-hex-character machine identifier <mid> and
// its source according to spec section 3: /etc/machine-id, then
// /var/lib/dbus/machine-id, then sha256(hostname)[:12].
func ResolveMachineID(paths []string, hostnameFn func() (string, error)) (string, string, error) {
	if len(paths) == 0 {
		paths = []string{MachineIDSourceEtc, MachineIDSourceDbus}
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if id, ok := extractHex12(string(b)); ok {
			return id, p, nil
		}
	}
	if hostnameFn == nil {
		hostnameFn = os.Hostname
	}
	host, err := hostnameFn()
	if err != nil || strings.TrimSpace(host) == "" {
		if err == nil {
			err = errors.New("empty hostname")
		}
		return "", "", fmt.Errorf("resolve machine id from hostname: %w", err)
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(host)))
	return hex.EncodeToString(sum[:6]), MachineIDSourceHostname, nil
}

func extractHex12(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "-", "")
	if len(s) < 12 {
		return "", false
	}
	cand := s[:12]
	if !hex12RE.MatchString(cand) {
		return "", false
	}
	return cand, true
}

// ResolveLayout computes all corrald paths for the current or configured user.
func ResolveLayout(opts PathOptions) (*Layout, error) {
	home := opts.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", err)
		}
	}
	if !filepath.IsAbs(home) || strings.ContainsAny(home, "\x00\n") {
		return nil, fmt.Errorf("invalid home directory %q", home)
	}
	home = filepath.Clean(home)

	uid := uint32(os.Geteuid())
	if opts.UID != nil {
		uid = *opts.UID
	}

	mid := opts.MachineID
	midSrc := opts.MachineIDSource
	if mid == "" {
		var err error
		mid, midSrc, err = ResolveMachineID(opts.MachineIDPaths, opts.HostnameFn)
		if err != nil {
			return nil, err
		}
	} else {
		mid = strings.ToLower(strings.TrimSpace(mid))
		if !hex12RE.MatchString(mid) {
			return nil, fmt.Errorf("invalid machine id %q: must be 12 lowercase hex characters", mid)
		}
		if midSrc == "" {
			midSrc = MachineIDSourceOverride
		}
	}

	tmpBase := opts.TmpBase
	if tmpBase == "" {
		tmpBase = "/tmp"
	}

	libDir := filepath.Join(home, ".local", "lib", "corral")
	stateRoot := filepath.Join(home, ".local", "state", "corral", "corrald")
	stateDir := filepath.Join(stateRoot, mid)

	sockPath := filepath.Join(stateDir, "corrald.sock")
	sockDir := stateDir
	usesTmp := false
	if len(sockPath) > MaxSocketPath {
		sum := sha256.Sum256([]byte(mid + home))
		sockDir = filepath.Join(tmpBase, fmt.Sprintf("corrald-%d-%s", uid, hex.EncodeToString(sum[:4])))
		sockPath = filepath.Join(sockDir, "corrald.sock")
		usesTmp = true
	}

	return &Layout{
		Home:             home,
		UID:              uid,
		MachineID:        mid,
		MachineIDSource:  midSrc,
		LibDir:           libDir,
		CurrentSymlink:   filepath.Join(libDir, "corrald"),
		ReadmePath:       filepath.Join(libDir, "README.txt"),
		SharedLockPath:   filepath.Join(libDir, ".corrald-shared.lock"),
		StateRoot:        stateRoot,
		StateDir:         stateDir,
		InstallJSON:      filepath.Join(stateDir, "install.json"),
		LockPath:         filepath.Join(stateDir, "corrald.lock"),
		OpLockPath:       filepath.Join(stateDir, "op.lock"),
		MetaJSON:         filepath.Join(stateDir, "meta.json"),
		ClientsJSON:      filepath.Join(stateDir, "clients.json"),
		LogPath:          filepath.Join(stateDir, "corrald.log"),
		OpsDir:           filepath.Join(stateDir, "ops"),
		TombstonePath:    filepath.Join(stateDir, "removing"),
		FailedJSON:       filepath.Join(stateDir, "failed.json"),
		SocketPath:       sockPath,
		SocketDir:        sockDir,
		SocketUsesTmpDir: usesTmp,
	}, nil
}

// VersionDir returns ~/.local/lib/corral/corrald-<version>-<release>.
func (l *Layout) VersionDir(version string, release int64) (string, error) {
	if err := validateVersionToken(version); err != nil {
		return "", err
	}
	if release < 0 {
		return "", fmt.Errorf("negative release %d", release)
	}
	return filepath.Join(l.LibDir, fmt.Sprintf("corrald-%s-%d", version, release)), nil
}

// VersionBinary returns ~/.local/lib/corral/corrald-<version>-<release>/corrald.
func (l *Layout) VersionBinary(version string, release int64) (string, error) {
	dir, err := l.VersionDir(version, release)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "corrald"), nil
}

// OpPath returns ~/.local/state/corral/corrald/<mid>/ops/<op>.json.
func (l *Layout) OpPath(opID string) (string, error) {
	if opID == "" || strings.ContainsAny(opID, "/\\\x00..") {
		return "", fmt.Errorf("invalid op id %q", opID)
	}
	return filepath.Join(l.OpsDir, opID+".json"), nil
}

// StageDir returns ~/.local/state/corral/corrald/<mid>/stage-<op>.
func (l *Layout) StageDir(opID string) (string, error) {
	if opID == "" || strings.ContainsAny(opID, "/\\\x00..") {
		return "", fmt.Errorf("invalid op id %q", opID)
	}
	return filepath.Join(l.StateDir, "stage-"+opID), nil
}

func validateVersionToken(version string) error {
	if !versionPartRE.MatchString(version) || strings.Contains(version, "..") {
		return fmt.Errorf("invalid version %q", version)
	}
	return nil
}

// ParseVersionDirName strictly validates a corrald version directory basename
// of the form "corrald-<Version>-<Release>" where Release is a non-negative
// decimal integer and Version is a valid version token. Unrelated directories
// such as "corrald-backups" return ok=false.
func ParseVersionDirName(name string) (version string, release int64, ok bool) {
	if !strings.HasPrefix(name, "corrald-") {
		return "", 0, false
	}
	rest := strings.TrimPrefix(name, "corrald-")
	idx := strings.LastIndexByte(rest, '-')
	if idx <= 0 || idx+1 >= len(rest) {
		return "", 0, false
	}
	ver := rest[:idx]
	relStr := rest[idx+1:]
	if err := validateVersionToken(ver); err != nil {
		return "", 0, false
	}
	if !digitsRE.MatchString(relStr) {
		return "", 0, false
	}
	var rel int64
	for i := 0; i < len(relStr); i++ {
		d := int64(relStr[i] - '0')
		if rel > (1<<62)/10 {
			return "", 0, false
		}
		rel = rel*10 + d
	}
	return ver, rel, true
}

// EnsureLibDir creates ~/.local/lib/corral if absent and verifies it is a real
// directory (not a symlink) owned by l.UID and not group/other writable.
func (l *Layout) EnsureLibDir() error {
	if err := os.MkdirAll(l.LibDir, 0o755); err != nil {
		return err
	}
	return CheckNonWritableDir(l.LibDir, l.UID)
}

// CheckNonWritableDir verifies dir is a real directory (not a symlink), owned
// by expectedUID, and not group- or world-writable (perm & 0022 == 0).
func CheckNonWritableDir(dir string, expectedUID uint32) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("directory %s is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %s is not a directory", dir)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat metadata unavailable for %s", dir)
	}
	if st.Uid != expectedUID {
		return fmt.Errorf("directory %s is owned by uid %d, expected %d", dir, st.Uid, expectedUID)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("directory %s is group/other writable (%04o)", dir, info.Mode().Perm())
	}
	return nil
}

// EnsureStateDir creates the host state directory (0700) and ops directory
// (0700), verifying that StateDir is a real directory owned by l.UID.
func (l *Layout) EnsureStateDir() error {
	if err := os.MkdirAll(l.StateDir, 0o700); err != nil {
		return err
	}
	if err := EnsureSecureDir(l.StateDir, l.UID); err != nil {
		return err
	}
	if err := os.MkdirAll(l.OpsDir, 0o700); err != nil {
		return err
	}
	return EnsureSecureDir(l.OpsDir, l.UID)
}

// EnsureSocketDir creates and validates the socket parent directory. When
// falling back to /tmp, it strictly checks that the directory is not a symlink,
// is owned by expectedUID, and has mode 0700.
func (l *Layout) EnsureSocketDir() error {
	if !l.SocketUsesTmpDir {
		return l.EnsureStateDir()
	}
	if err := os.Mkdir(l.SocketDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create socket dir %s: %w", l.SocketDir, err)
	}
	return EnsureSecureDir(l.SocketDir, l.UID)
}

// EnsureSecureDir verifies dir is a real directory (not a symlink), owned by
// expectedUID, and restricted to mode 0700. If it is owned by another user or
// is a symlink or non-directory, it returns an error without modifying it.
func EnsureSecureDir(dir string, expectedUID uint32) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("directory %s is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %s is not a directory", dir)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat metadata unavailable for %s", dir)
	}
	if st.Uid != expectedUID {
		return fmt.Errorf("directory %s is owned by uid %d, expected %d", dir, st.Uid, expectedUID)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod 0700 %s: %w", dir, err)
		}
		info2, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("re-lstat %s: %w", dir, err)
		}
		st2, ok := info2.Sys().(*syscall.Stat_t)
		if !ok || info2.Mode()&os.ModeSymlink != 0 || !info2.IsDir() || st2.Uid != expectedUID || info2.Mode().Perm() != 0o700 {
			return fmt.Errorf("directory %s failed post-chmod verification", dir)
		}
	}
	return nil
}

// TombstoneExists reports whether the removing tombstone file exists.
func (l *Layout) TombstoneExists() bool {
	_, err := os.Lstat(l.TombstonePath)
	return err == nil
}
