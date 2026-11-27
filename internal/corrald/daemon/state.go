package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"
)

// InstallRecord is persisted at ~/.local/state/corral/corrald/<mid>/install.json.
type InstallRecord struct {
	Version         string   `json:"version"`
	Release         int64    `json:"release"`
	Commit          string   `json:"commit,omitempty"`
	Gateway         string   `json:"gateway"`
	EnabledBy       string   `json:"enabled_by"`
	EnabledAt       string   `json:"enabled_at"`
	MachineID       string   `json:"machine_id"`
	MachineIDSource string   `json:"machine_id_source"`
	Files           []string `json:"files"`
}

// MetaRecord is persisted at ~/.local/state/corral/corrald/<mid>/meta.json
// while the daemon is serving.
type MetaRecord struct {
	PID           int    `json:"pid"`
	StartTime     string `json:"start_time"`
	PrevPID       int    `json:"prev_pid,omitempty"`
	PrevStartTime string `json:"prev_start_time,omitempty"`
	Version       string `json:"version"`
	Release       int64  `json:"release"`
	Commit        string `json:"commit,omitempty"`
	Epoch         string `json:"epoch"`
	StartedAt     string `json:"started_at"`
	SocketPath    string `json:"socket_path"`
	Gateway       string `json:"gateway,omitempty"`
}

// OpRecordFile is persisted at ~/.local/state/corral/corrald/<mid>/ops/<op>.json.
type OpRecordFile struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
	Release int64  `json:"release,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	By      string `json:"by,omitempty"`
	At      string `json:"at"`
	Result  string `json:"result"`
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// FailedEntry records a single release that failed to start or hand off on
// this host.
type FailedEntry struct {
	Release int64  `json:"release"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
}

// FailedRecord is persisted at ~/.local/state/corral/corrald/<mid>/failed.json.
type FailedRecord struct {
	Releases []FailedEntry `json:"releases"`
}

// ReadInstallRecord loads install.json if present.
func ReadInstallRecord(path string) (*InstallRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec InstallRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// WriteInstallRecord atomically writes install.json (mode 0600).
func WriteInstallRecord(path string, rec *InstallRecord) error {
	rec.Files = dedupSortedStrings(rec.Files)
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileAtomic(path, b, 0o600)
}

// ReadMetaRecord loads meta.json if present.
func ReadMetaRecord(path string) (*MetaRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec MetaRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// WriteMetaRecord atomically writes meta.json (mode 0600).
func WriteMetaRecord(path string, rec *MetaRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileAtomic(path, b, 0o600)
}

// WriteOpRecord atomically writes ops/<op>.json (mode 0600).
func WriteOpRecord(path string, rec *OpRecordFile) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileAtomic(path, b, 0o600)
}

// ReadFailedRecord loads failed.json, returning an empty record if missing.
func ReadFailedRecord(path string) (*FailedRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &FailedRecord{Releases: []FailedEntry{}}, nil
		}
		return nil, err
	}
	var rec FailedRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	if rec.Releases == nil {
		rec.Releases = []FailedEntry{}
	}
	return &rec, nil
}

// RecordFailedRelease appends or updates release in failed.json.
func RecordFailedRelease(path string, release int64, reason string) error {
	rec, err := ReadFailedRecord(path)
	if err != nil {
		rec = &FailedRecord{Releases: []FailedEntry{}}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	updated := false
	for i := range rec.Releases {
		if rec.Releases[i].Release == release {
			rec.Releases[i].Reason = reason
			rec.Releases[i].At = now
			updated = true
			break
		}
	}
	if !updated {
		rec.Releases = append(rec.Releases, FailedEntry{
			Release: release,
			Reason:  reason,
			At:      now,
		})
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileAtomic(path, b, 0o600)
}

func dedupSortedStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
