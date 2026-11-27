// Package proto defines the wire protocol, message envelopes, version and
// release comparisons, capability manifest, and bilingual README template
// shared by corrald (on target hosts) and the Corral gateway.
package proto

import (
	"strconv"
	"strings"
)

// Protocol is the integer wire protocol version spoken by corrald and the
// gateway in stage 1.
const Protocol = 1

// ProtoMin and ProtoMax bound the protocol versions supported by stage 1.
const (
	ProtoMin = 1
	ProtoMax = 1
)

// Build metadata variables. They may be injected at link time via -ldflags -X
// or populated by cmd/corrald at startup. When unset, Version defaults to
// "dev" and Release defaults to 0.
var (
	Version    = "dev"
	ReleaseStr = "0"
	Commit     = ""
	Release    int64
)

func init() {
	if v, err := ParseRelease(ReleaseStr); err == nil {
		Release = v
	}
}

// BuildInfo carries the three version fields of a corrald binary.
type BuildInfo struct {
	Version string `json:"version"`
	Release int64  `json:"release"`
	Commit  string `json:"commit"`
}

// CurrentBuildInfo returns the build metadata currently configured in proto.
func CurrentBuildInfo() BuildInfo {
	v := Version
	if strings.TrimSpace(v) == "" {
		v = "dev"
	}
	return BuildInfo{
		Version: v,
		Release: Release,
		Commit:  Commit,
	}
}

// ParseRelease parses a decimal Unix-second release timestamp string.
// Empty strings parse as 0. Negative values are rejected.
func ParseRelease(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, strconv.ErrRange
	}
	return n, nil
}

// CompareRelease compares two release timestamps (Unix seconds).
// It returns -1 if a < b, 0 if a == b, and +1 if a > b.
func CompareRelease(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// ShouldUpgrade reports whether candidate should replace current.
// Upgrades are strictly monotonic: only a strictly greater release qualifies;
// equal or older releases never trigger an automatic upgrade or downgrade.
func ShouldUpgrade(current, candidate int64) bool {
	return CompareRelease(candidate, current) > 0
}

// ProtocolInRange reports whether proto falls within the inclusive gateway
// compatibility interval [minProto, maxProto].
func ProtocolInRange(proto, minProto, maxProto int) bool {
	return minProto > 0 && maxProto >= minProto && proto >= minProto && proto <= maxProto
}
