package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OwnedLeftover describes a file or directory that RemoveOwned did not delete
// (either because it is not owned by corrald, is an unsafe symlinked directory
// root, or could not be removed).
type OwnedLeftover struct {
	Path   string
	Reason string
}

// OwnedRemoveKind selects which owned-path category RemoveOwned operates on.
type OwnedRemoveKind int

const (
	// OwnedRemoveLibTarget removes a single corrald-owned target directly under
	// layout.LibDir (corrald symlink, README.txt, owned temp file, or a version
	// directory validated by ParseVersionDirName).
	OwnedRemoveLibTarget OwnedRemoveKind = iota
	// OwnedRemoveVersionDir removes owned files inside a ParseVersionDirName
	// directory under layout.LibDir and rmdirs it if empty.
	OwnedRemoveVersionDir
	// OwnedRemoveStageDir removes owned upload/staging files inside a
	// stage-<op> directory under layout.StateDir and rmdirs it if empty.
	OwnedRemoveStageDir
	// OwnedRemoveOpsDir removes owned <opid>.json (and temp) files inside
	// layout.OpsDir and rmdirs it if empty.
	OwnedRemoveOpsDir
	// OwnedRemoveStateEntries removes all owned non-lock/non-tombstone entries
	// inside layout.StateDir (leaving op.lock, corrald.lock, removing intact).
	OwnedRemoveStateEntries
)

// IsOwnedOpID reports whether id matches corrald's exact generated operation ID
// format (16 lowercase hex characters from 8 random bytes).
func IsOwnedOpID(id string) bool {
	return isHexLen(id, 16)
}

// IsOwnedStageDirName reports whether basename matches corrald's exact stage
// directory format ("stage-" + 16 lowercase hex characters).
func IsOwnedStageDirName(name string) bool {
	if !strings.HasPrefix(name, "stage-") {
		return false
	}
	return IsOwnedOpID(strings.TrimPrefix(name, "stage-"))
}

// ManifestFileSet builds a cleaned path lookup set from install.json's Files.
func ManifestFileSet(rec *InstallRecord) map[string]struct{} {
	if rec == nil || len(rec.Files) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(rec.Files))
	for _, f := range rec.Files {
		if f != "" {
			set[filepath.Clean(f)] = struct{}{}
		}
	}
	return set
}

func isHexLen(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if len(s) == 0 || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isOwnedTempForBase matches the EXACT temp-file names written by corrald:
//   - WriteFileAtomic / AtomicSymlink / copyBinaryAtomic: ".<base>.tmp.<pid>.<8hex>"
//   - sftpWriteFile: ".<base>.tmp.<16hex>"
func isOwnedTempForBase(name, base string) bool {
	prefix := "." + base + ".tmp."
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rem := strings.TrimPrefix(name, prefix)
	if isHexLen(rem, 16) {
		return true
	}
	dot := strings.IndexByte(rem, '.')
	if dot <= 0 || dot+1 >= len(rem) {
		return false
	}
	return isDigits(rem[:dot]) && isHexLen(rem[dot+1:], 8)
}

// isOwnedStageUploadTemp matches sftpUploadBinarySecure's ".corrald.upload.<16hex>".
func isOwnedStageUploadTemp(name string) bool {
	if !strings.HasPrefix(name, ".corrald.upload.") {
		return false
	}
	return isHexLen(strings.TrimPrefix(name, ".corrald.upload."), 16)
}

// isOwnedSelftestLock matches cmdSelftest's ".selftest-<pid>.lock".
func isOwnedSelftestLock(name string) bool {
	if !strings.HasPrefix(name, ".selftest-") || !strings.HasSuffix(name, ".lock") {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, ".selftest-"), ".lock")
	return isDigits(mid)
}

var fixedStateTempBases = []string{
	"install.json",
	"meta.json",
	"clients.json",
	"failed.json",
	"removing",
}

func isOwnedStateFileName(name string) bool {
	switch name {
	case "install.json", "meta.json", "clients.json", "failed.json",
		"corrald.log", "corrald.log.1", "corrald.sock":
		return true
	}
	if isOwnedSelftestLock(name) {
		return true
	}
	for _, base := range fixedStateTempBases {
		if isOwnedTempForBase(name, base) {
			return true
		}
	}
	return false
}

func isOwnedLibFileName(name string) bool {
	if name == "corrald" || name == "README.txt" {
		return true
	}
	return isOwnedTempForBase(name, "corrald") || isOwnedTempForBase(name, "README.txt")
}

func isOwnedOpsFileName(layout *Layout, name string, manifestFiles map[string]struct{}) bool {
	if strings.HasSuffix(name, ".json") {
		opID := strings.TrimSuffix(name, ".json")
		if IsOwnedOpID(opID) {
			return true
		}
		if manifestFiles != nil {
			if _, ok := manifestFiles[filepath.Clean(filepath.Join(layout.OpsDir, name))]; ok {
				return true
			}
		}
		return false
	}
	if strings.HasPrefix(name, ".") {
		idx := strings.Index(name[1:], ".tmp.")
		if idx > 0 {
			base := name[1 : 1+idx]
			if strings.HasSuffix(base, ".json") && isOwnedOpsFileName(layout, base, manifestFiles) && isOwnedTempForBase(name, base) {
				return true
			}
		}
	}
	return false
}

func isOwnedVersionFileName(name string) bool {
	return name == "corrald" || isOwnedTempForBase(name, "corrald")
}

func isOwnedStageFileName(name string) bool {
	return name == "corrald" || isOwnedStageUploadTemp(name) || isOwnedTempForBase(name, "corrald")
}

// RemoveOwned is the SINGLE owned-removal implementation used by every
// uninstall, recovery, version-pruning, and stage-cleanup path in corrald.
//
// Invariants:
//   - Never follows symlinks: Lstats every directory root and every entry;
//     symlinked directory roots (StateDir, ops, stage-*, LibDir version dirs)
//     are never descended into and are reported as leftovers.
//   - Only removes entries matching corrald's exact naming formats (or explicit
//     owned manifest entries).
//   - Never calls os.RemoveAll: removes owned files one-by-one, then rmdirs
//     owned directories; any unknown/user file survives and is reported.
func RemoveOwned(layout *Layout, kind OwnedRemoveKind, target string, manifestFiles map[string]struct{}) []OwnedLeftover {
	if layout == nil {
		return nil
	}
	var left []OwnedLeftover
	seen := make(map[string]struct{})
	add := func(p, reason string) {
		cp := filepath.Clean(p)
		if _, ok := seen[cp]; ok {
			return
		}
		seen[cp] = struct{}{}
		left = append(left, OwnedLeftover{Path: cp, Reason: strings.ReplaceAll(reason, "\n", " ")})
	}

	removeOwnedFile := func(p string) {
		info, err := os.Lstat(p)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				add(p, err.Error())
			}
			return
		}
		// Owned file entry: unlink regular file, socket, or symlink (never
		// traverse); refuse to recursively delete if replaced by a directory.
		if info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
			add(p, "expected file but found directory; preserved")
			return
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			add(p, err.Error())
		}
	}

	removeOwnedDir := func(dir string, isOwnedChild func(name string) bool, preserveReason string) {
		info, err := os.Lstat(dir)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				add(dir, err.Error())
			}
			return
		}
		if info.Mode()&os.ModeSymlink != 0 {
			add(dir, "symlinked directory root not traversed")
			return
		}
		if !info.IsDir() {
			add(dir, "expected directory, found non-directory")
			return
		}
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			add(dir, rerr.Error())
			return
		}
		for _, e := range entries {
			name := e.Name()
			child := filepath.Join(dir, name)
			if isOwnedChild(name) {
				removeOwnedFile(child)
			}
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			if rem, rerr := os.ReadDir(dir); rerr == nil && len(rem) > 0 {
				for _, e := range rem {
					add(filepath.Join(dir, e.Name()), preserveReason)
				}
			} else {
				add(dir, err.Error())
			}
		}
	}

	switch kind {
	case OwnedRemoveLibTarget:
		cleanLib := filepath.Clean(layout.LibDir)
		clean := filepath.Clean(target)
		if filepath.Dir(clean) != cleanLib {
			return nil
		}
		// Never descend into a symlinked LibDir root.
		if linfo, err := os.Lstat(cleanLib); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				add(cleanLib, err.Error())
			}
			return left
		} else if linfo.Mode()&os.ModeSymlink != 0 || !linfo.IsDir() {
			add(cleanLib, "symlinked or non-directory lib dir not traversed")
			return left
		}
		base := filepath.Base(clean)
		if isOwnedLibFileName(base) {
			removeOwnedFile(clean)
			return left
		}
		if _, _, ok := ParseVersionDirName(base); ok {
			removeOwnedDir(clean, isOwnedVersionFileName, "unexpected file in corrald version dir preserved")
		}

	case OwnedRemoveVersionDir:
		cleanLib := filepath.Clean(layout.LibDir)
		clean := filepath.Clean(target)
		if filepath.Dir(clean) != cleanLib {
			return nil
		}
		if _, _, ok := ParseVersionDirName(filepath.Base(clean)); !ok {
			return nil
		}
		if linfo, err := os.Lstat(cleanLib); err != nil || linfo.Mode()&os.ModeSymlink != 0 || !linfo.IsDir() {
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				add(cleanLib, err.Error())
			} else if err == nil {
				add(cleanLib, "symlinked or non-directory lib dir not traversed")
			}
			return left
		}
		removeOwnedDir(clean, isOwnedVersionFileName, "unexpected file in corrald version dir preserved")

	case OwnedRemoveStageDir:
		cleanState := filepath.Clean(layout.StateDir)
		clean := filepath.Clean(target)
		if filepath.Dir(clean) != cleanState || !strings.HasPrefix(filepath.Base(clean), "stage-") {
			add(clean, "path is not a valid stage directory")
			return left
		}
		if sinfo, err := os.Lstat(cleanState); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				add(cleanState, err.Error())
			}
			return left
		} else if sinfo.Mode()&os.ModeSymlink != 0 || !sinfo.IsDir() {
			add(cleanState, "symlinked or non-directory state dir not traversed")
			return left
		}
		removeOwnedDir(clean, isOwnedStageFileName, "unexpected file in stage dir preserved")

	case OwnedRemoveOpsDir:
		cleanOps := filepath.Clean(layout.OpsDir)
		removeOwnedDir(cleanOps, func(name string) bool {
			return isOwnedOpsFileName(layout, name, manifestFiles)
		}, "unexpected file in ops dir preserved")

	case OwnedRemoveStateEntries:
		cleanState := filepath.Clean(layout.StateDir)
		sinfo, err := os.Lstat(cleanState)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				add(cleanState, err.Error())
			}
			return left
		}
		if sinfo.Mode()&os.ModeSymlink != 0 {
			add(cleanState, "symlinked state dir not traversed")
			return left
		}
		if !sinfo.IsDir() {
			add(cleanState, "state dir is not a directory")
			return left
		}
		entries, rerr := os.ReadDir(cleanState)
		if rerr != nil {
			add(cleanState, rerr.Error())
			return left
		}
		for _, e := range entries {
			name := e.Name()
			if name == "op.lock" || name == "corrald.lock" || name == "removing" {
				continue
			}
			p := filepath.Join(cleanState, name)
			switch {
			case isOwnedStateFileName(name):
				removeOwnedFile(p)
			case name == "ops":
				for _, l := range RemoveOwned(layout, OwnedRemoveOpsDir, p, manifestFiles) {
					add(l.Path, l.Reason)
				}
			case IsOwnedStageDirName(name):
				for _, l := range RemoveOwned(layout, OwnedRemoveStageDir, p, manifestFiles) {
					add(l.Path, l.Reason)
				}
			default:
				// Unowned user file or directory (e.g. .notes.tmp.backup,
				// stage-backups/): leave in place and report as leftover.
				add(p, "unowned entry in state dir preserved")
			}
		}
	}

	return left
}

// isSiblingTombstoneOnlyDir reports whether stateRoot/<entryName> is another
// machine's state directory that has NO install.json, NO live daemon holding
// corrald.lock, and holds a removing tombstone (Fix11 Item #7).
// Such a sibling directory is in the middle of its own uninstall/retry and must
// not block this machine's uninstall or prevent removal of shared binaries.
func isSiblingTombstoneOnlyDir(stateRoot, currentMID, entryName string) bool {
	if entryName == "" || entryName == currentMID {
		return false
	}
	dir := filepath.Join(stateRoot, entryName)
	dinfo, err := os.Lstat(dir)
	if err != nil || dinfo.Mode()&os.ModeSymlink != 0 || !dinfo.IsDir() {
		return false
	}
	if _, err := os.Lstat(filepath.Join(dir, "install.json")); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	tinfo, err := os.Lstat(filepath.Join(dir, "removing"))
	if err != nil || !tinfo.Mode().IsRegular() {
		return false
	}
	lockPath := filepath.Join(dir, "corrald.lock")
	if _, err := os.Lstat(lockPath); err == nil {
		ll, lerr := AcquireLock(lockPath)
		if lerr != nil {
			// Live daemon (or active uninstall) holds corrald.lock.
			return false
		}
		_ = ll.Release()
	} else if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true
}

func formatOwnedLeftoverErr(left []OwnedLeftover) error {
	if len(left) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s", left[0].Path, left[0].Reason)
}
