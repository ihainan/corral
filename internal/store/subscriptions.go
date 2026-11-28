package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

const maxSubscriptionHosts = 20

// SubscriptionHost records one host on which a subscription account was seen.
type SubscriptionHost struct {
	HostID   int64  `json:"host_id"`
	HostName string `json:"host_name"`
	SeenAt   int64  `json:"seen_at"`
}

// SubscriptionSnapshot stores the latest observed quota snapshot for one
// (owner, provider, account_key) tuple.
type SubscriptionSnapshot struct {
	OwnerID       int64              `json:"-"`
	Provider      string             `json:"provider"`
	AccountKey    string             `json:"account_key"`
	AccountLabel  string             `json:"account_label,omitempty"`
	Plan          string             `json:"plan,omitempty"`
	HostID        int64              `json:"host_id"`
	HostName      string             `json:"host_name"`
	Hosts         []SubscriptionHost `json:"hosts"`
	QuotaJSON     json.RawMessage    `json:"quota"`
	UpdatedAt     int64              `json:"updated_at"`
	SeenAt        int64              `json:"seen_at"`
	LastRefreshAt int64              `json:"last_refresh_at,omitempty"`
}

func mergeSubscriptionHosts(existing []SubscriptionHost, hostID int64, hostName string, seenAt int64) []SubscriptionHost {
	if hostID <= 0 && hostName == "" {
		return existing
	}
	byID := make(map[int64]SubscriptionHost, len(existing)+1)
	var extra []SubscriptionHost
	for _, h := range existing {
		if h.HostID > 0 {
			if prev, ok := byID[h.HostID]; !ok || h.SeenAt > prev.SeenAt {
				byID[h.HostID] = h
			}
		} else if h.HostName != "" {
			extra = append(extra, h)
		}
	}
	if hostID > 0 {
		prev := byID[hostID]
		name := strings.TrimSpace(hostName)
		if name == "" {
			name = prev.HostName
		}
		ts := seenAt
		if prev.SeenAt > ts {
			ts = prev.SeenAt
		}
		byID[hostID] = SubscriptionHost{HostID: hostID, HostName: name, SeenAt: ts}
	} else if hostName != "" {
		found := false
		for i := range extra {
			if extra[i].HostName == hostName {
				if seenAt > extra[i].SeenAt {
					extra[i].SeenAt = seenAt
				}
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, SubscriptionHost{HostName: hostName, SeenAt: seenAt})
		}
	}
	out := make([]SubscriptionHost, 0, len(byID)+len(extra))
	for _, h := range byID {
		out = append(out, h)
	}
	out = append(out, extra...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].SeenAt != out[j].SeenAt {
			return out[i].SeenAt > out[j].SeenAt
		}
		return out[i].HostName < out[j].HostName
	})
	if len(out) > maxSubscriptionHosts {
		out = out[:maxSubscriptionHosts]
	}
	return out
}

// UpsertSubscriptionSnapshot records a passively observed or refreshed quota
// snapshot for (owner_id, provider, account_key). Older quota payloads never
// overwrite a newer snapshot, while host lists and missing labels/plans are
// still merged.
func (s *Store) UpsertSubscriptionSnapshot(ctx context.Context, in SubscriptionSnapshot) error {
	if in.OwnerID <= 0 || in.Provider == "" || in.AccountKey == "" {
		return errors.New("subscription snapshot requires owner, provider and account key")
	}
	now := s.unix()
	seenAt := in.SeenAt
	if seenAt <= 0 {
		seenAt = now
	}
	updatedAt := in.UpdatedAt
	if updatedAt <= 0 {
		updatedAt = seenAt
	}
	quotaStr := "{}"
	if len(in.QuotaJSON) > 0 {
		quotaStr = string(in.QuotaJSON)
	}

	var (
		exLabel, exPlan, exHostName, exHostsJSON, exQuotaJSON string
		exHostID, exUpdatedAt, exSeenAt, exLastRefresh        int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT account_label, plan, host_id, host_name, hosts_json, quota_json, updated_at, seen_at, last_refresh_at
		 FROM subscription_snapshots
		 WHERE owner_id = ? AND provider = ? AND account_key = ?`,
		in.OwnerID, in.Provider, in.AccountKey,
	).Scan(&exLabel, &exPlan, &exHostID, &exHostName, &exHostsJSON, &exQuotaJSON, &exUpdatedAt, &exSeenAt, &exLastRefresh)

	if errors.Is(err, sql.ErrNoRows) {
		hosts := mergeSubscriptionHosts(in.Hosts, in.HostID, in.HostName, seenAt)
		hostsBytes, err := json.Marshal(hosts)
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO subscription_snapshots
			 (owner_id, provider, account_key, account_label, plan, host_id, host_name, hosts_json, quota_json, updated_at, seen_at, last_refresh_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.OwnerID, in.Provider, in.AccountKey, in.AccountLabel, in.Plan,
			in.HostID, in.HostName, string(hostsBytes), quotaStr, updatedAt, seenAt, in.LastRefreshAt,
		)
		return err
	}
	if err != nil {
		return err
	}

	var existingHosts []SubscriptionHost
	_ = json.Unmarshal([]byte(exHostsJSON), &existingHosts)
	mergedHosts := mergeSubscriptionHosts(existingHosts, in.HostID, in.HostName, seenAt)
	hostsBytes, err := json.Marshal(mergedHosts)
	if err != nil {
		return err
	}

	finalLabel := exLabel
	if strings.TrimSpace(in.AccountLabel) != "" {
		finalLabel = strings.TrimSpace(in.AccountLabel)
	}
	finalPlan := exPlan
	if strings.TrimSpace(in.Plan) != "" {
		finalPlan = strings.TrimSpace(in.Plan)
	}
	finalHostID := exHostID
	finalHostName := exHostName
	finalQuota := exQuotaJSON
	finalUpdatedAt := exUpdatedAt
	if updatedAt >= exUpdatedAt {
		finalQuota = quotaStr
		finalUpdatedAt = updatedAt
		if in.HostID > 0 {
			finalHostID = in.HostID
		}
		if in.HostName != "" {
			finalHostName = in.HostName
		}
	}
	finalSeenAt := exSeenAt
	if seenAt > finalSeenAt {
		finalSeenAt = seenAt
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE subscription_snapshots
		 SET account_label = ?, plan = ?, host_id = ?, host_name = ?, hosts_json = ?, quota_json = ?, updated_at = ?, seen_at = ?
		 WHERE owner_id = ? AND provider = ? AND account_key = ?`,
		finalLabel, finalPlan, finalHostID, finalHostName, string(hostsBytes), finalQuota, finalUpdatedAt, finalSeenAt,
		in.OwnerID, in.Provider, in.AccountKey,
	)
	return err
}

func scanSubscriptionSnapshot(row interface{ Scan(...any) error }) (*SubscriptionSnapshot, error) {
	sn := &SubscriptionSnapshot{}
	var hostsJSON, quotaJSON string
	if err := row.Scan(
		&sn.OwnerID, &sn.Provider, &sn.AccountKey, &sn.AccountLabel, &sn.Plan,
		&sn.HostID, &sn.HostName, &hostsJSON, &quotaJSON,
		&sn.UpdatedAt, &sn.SeenAt, &sn.LastRefreshAt,
	); err != nil {
		return nil, notFound(err)
	}
	if hostsJSON != "" {
		_ = json.Unmarshal([]byte(hostsJSON), &sn.Hosts)
	}
	if sn.Hosts == nil {
		sn.Hosts = []SubscriptionHost{}
	}
	if quotaJSON == "" {
		quotaJSON = "{}"
	}
	sn.QuotaJSON = json.RawMessage(quotaJSON)
	return sn, nil
}

// ListSubscriptionSnapshots returns all observed subscription snapshots for ownerID,
// newest updated_at first.
func (s *Store) ListSubscriptionSnapshots(ctx context.Context, ownerID int64) ([]*SubscriptionSnapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT owner_id, provider, account_key, account_label, plan, host_id, host_name, hosts_json, quota_json, updated_at, seen_at, last_refresh_at
		 FROM subscription_snapshots
		 WHERE owner_id = ?
		 ORDER BY updated_at DESC, provider ASC, account_key ASC`,
		ownerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SubscriptionSnapshot{}
	for rows.Next() {
		sn, err := scanSubscriptionSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// GetSubscriptionSnapshot returns a single snapshot owned by ownerID.
func (s *Store) GetSubscriptionSnapshot(ctx context.Context, ownerID int64, provider, accountKey string) (*SubscriptionSnapshot, error) {
	return scanSubscriptionSnapshot(s.db.QueryRowContext(ctx,
		`SELECT owner_id, provider, account_key, account_label, plan, host_id, host_name, hosts_json, quota_json, updated_at, seen_at, last_refresh_at
		 FROM subscription_snapshots
		 WHERE owner_id = ? AND provider = ? AND account_key = ?`,
		ownerID, provider, accountKey,
	))
}

// TryClaimSubscriptionRefresh enforces a per-account minimum cooldown (in
// seconds) on manual refreshes. When allowed is true, last_refresh_at is
// atomically updated to now; otherwise retryAfter gives the remaining wait.
func (s *Store) TryClaimSubscriptionRefresh(ctx context.Context, ownerID int64, provider, accountKey string, cooldownSec int64) (allowed bool, retryAfter int64, nextAt int64, err error) {
	now := s.unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE subscription_snapshots
		 SET last_refresh_at = ?
		 WHERE owner_id = ? AND provider = ? AND account_key = ?
		   AND (last_refresh_at = 0 OR (? - last_refresh_at) >= ?)`,
		now, ownerID, provider, accountKey, now, cooldownSec,
	)
	if err != nil {
		return false, 0, 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, 0, now + cooldownSec, nil
	}
	var last int64
	err = s.db.QueryRowContext(ctx,
		`SELECT last_refresh_at FROM subscription_snapshots
		 WHERE owner_id = ? AND provider = ? AND account_key = ?`,
		ownerID, provider, accountKey,
	).Scan(&last)
	if err != nil {
		return false, 0, 0, notFound(err)
	}
	nextAt = last + cooldownSec
	retryAfter = nextAt - now
	if retryAfter < 1 {
		retryAfter = 1
	}
	return false, retryAfter, nextAt, nil
}
