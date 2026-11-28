package gw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// ErrDistUnavailable is returned when the bundled corrald binaries are missing
// or unusable on the gateway.
var ErrDistUnavailable = errors.New("corrald binaries are not available on this gateway")

// ArchBinary describes one architecture's bundled corrald executable.
type ArchBinary struct {
	Arch   string
	Path   string
	SHA256 string
	Size   int64
}

// Dist holds the loaded corrald distribution binaries, checksums, version
// metadata, capability manifest, and release changelog served by the gateway.
type Dist struct {
	Dir          string
	Version      string
	Release      int64
	Commit       string
	Protocol     int
	Manifest     proto.Manifest
	ManifestHash string
	Changelog    []proto.ChangelogEntry
	Binaries     map[string]*ArchBinary
}

type distMetaFile struct {
	Version  string `json:"version"`
	Release  int64  `json:"release"`
	Commit   string `json:"commit"`
	Protocol int    `json:"protocol"`
}

// SupportedArches lists the target Linux architectures bundled by the gateway.
var SupportedArches = []string{"amd64", "arm64"}

// LoadDist inspects dir for linux-amd64/corrald and linux-arm64/corrald,
// computes their SHA-256 digests, and reads version metadata and the capability
// manifest (from manifest.json / version.json if present, otherwise by running
// the native-architecture binary on Linux).
func LoadDist(dir string) (*Dist, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, ErrDistUnavailable
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrDistUnavailable, dir)
	}

	bins := make(map[string]*ArchBinary, len(SupportedArches))
	for _, arch := range SupportedArches {
		p := filepath.Join(dir, "linux-"+arch, "corrald")
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("%w: missing %s", ErrDistUnavailable, p)
		}
		sum, err := hashFileSHA256(p)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", p, err)
		}
		bins[arch] = &ArchBinary{
			Arch:   arch,
			Path:   p,
			SHA256: sum,
			Size:   info.Size(),
		}
	}

	d := &Dist{
		Dir:      dir,
		Version:  "dev",
		Protocol: proto.Protocol,
		Binaries: bins,
	}

	// Read version metadata: prefer version.json if present (e.g. cross-arch builds
	// or tests), otherwise execute the native Linux binary's `version` subcommand.
	metaPath := filepath.Join(dir, "version.json")
	if raw, err := os.ReadFile(metaPath); err == nil {
		var m distMetaFile
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("parse %s: %w", metaPath, err)
		}
		if m.Version != "" {
			d.Version = m.Version
		}
		d.Release = m.Release
		d.Commit = m.Commit
		if m.Protocol > 0 {
			d.Protocol = m.Protocol
		}
	} else if native, ok := nativeBinary(bins); ok {
		if fields, err := runLocalBinary(native.Path, "version"); err == nil && fields["result"] == "ok" {
			if fields["version"] != "" {
				d.Version = fields["version"]
			}
			if rel, err := proto.ParseRelease(fields["release"]); err == nil {
				d.Release = rel
			}
			d.Commit = fields["commit"]
			if p, err := strconv.Atoi(fields["protocol"]); err == nil && p > 0 {
				d.Protocol = p
			}
		}
	}

	// Read capability manifest: prefer manifest.json sidecar if present,
	// otherwise execute the native Linux binary's `manifest` subcommand.
	manifestPath := filepath.Join(dir, "manifest.json")
	if raw, err := os.ReadFile(manifestPath); err == nil {
		var m proto.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("parse %s: %w", manifestPath, err)
		}
		d.Manifest = m
	} else if native, ok := nativeBinary(bins); ok {
		out, err := runLocalBinaryRaw(native.Path, "manifest")
		if err != nil {
			return nil, fmt.Errorf("read manifest from %s: %w", native.Path, err)
		}
		var m proto.Manifest
		if err := json.Unmarshal(bytes.TrimSpace(out), &m); err != nil {
			return nil, fmt.Errorf("parse manifest from %s: %w", native.Path, err)
		}
		d.Manifest = m
	} else {
		return nil, fmt.Errorf("%w: manifest.json missing and cannot execute linux binary on %s/%s", ErrDistUnavailable, runtime.GOOS, runtime.GOARCH)
	}

	mh, err := proto.ManifestSHA256(d.Manifest)
	if err != nil {
		return nil, fmt.Errorf("hash manifest: %w", err)
	}
	d.ManifestHash = mh

	// Read release changelog: prefer changelog.json sidecar if present,
	// otherwise execute the native Linux binary's `changelog` subcommand,
	// falling back to the compiled-in proto.Changelog().
	changelogPath := filepath.Join(dir, "changelog.json")
	if raw, err := os.ReadFile(changelogPath); err == nil {
		var entries []proto.ChangelogEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("parse %s: %w", changelogPath, err)
		}
		d.Changelog = entries
	} else if native, ok := nativeBinary(bins); ok {
		if out, err := runLocalBinaryRaw(native.Path, "changelog"); err == nil {
			var entries []proto.ChangelogEntry
			if json.Unmarshal(bytes.TrimSpace(out), &entries) == nil {
				d.Changelog = entries
			}
		}
	}
	if d.Changelog == nil {
		d.Changelog = proto.Changelog()
	}

	return d, nil
}

// Binary returns the distribution binary for arch ("amd64" or "arm64").
func (d *Dist) Binary(arch string) (*ArchBinary, bool) {
	if d == nil {
		return nil, false
	}
	b, ok := d.Binaries[arch]
	return b, ok
}

// EstimatedSizeMB returns the rounded-up megabyte size of the largest bundled binary.
func (d *Dist) EstimatedSizeMB() int {
	if d == nil {
		return 10
	}
	var maxBytes int64
	for _, b := range d.Binaries {
		if b.Size > maxBytes {
			maxBytes = b.Size
		}
	}
	if maxBytes <= 0 {
		return 10
	}
	mb := int((maxBytes + (1 << 20) - 1) >> 20)
	if mb < 1 {
		mb = 1
	}
	return mb
}

func nativeBinary(bins map[string]*ArchBinary) (*ArchBinary, bool) {
	if runtime.GOOS != "linux" {
		return nil, false
	}
	b, ok := bins[runtime.GOARCH]
	return b, ok
}

func runLocalBinaryRaw(binPath string, arg string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, arg)
	return cmd.Output()
}

func runLocalBinary(binPath string, arg string) (map[string]string, error) {
	out, err := runLocalBinaryRaw(binPath, arg)
	if err != nil {
		return nil, err
	}
	return ParseKeyValues(out), nil
}

func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
