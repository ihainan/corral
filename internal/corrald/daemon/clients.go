package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

type clientKey struct {
	gateway    string
	corralUser string
}

// ClientRegistry maintains registered gateway/user pairs and persists them to
// ~/.local/state/corral/corrald/<mid>/clients.json.
type ClientRegistry struct {
	mu      sync.Mutex
	path    string
	clients map[clientKey]proto.ClientInfo
}

// OpenClientRegistry loads existing clients from path (if present) and returns
// a registry that persists subsequent mutations atomically.
func OpenClientRegistry(path string) (*ClientRegistry, error) {
	r := &ClientRegistry{
		path:    path,
		clients: make(map[clientKey]proto.ClientInfo),
	}
	if path == "" {
		return r, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return nil, err
	}
	var list []proto.ClientInfo
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, c := range list {
		if c.Gateway == "" && c.CorralUser == "" {
			continue
		}
		r.clients[clientKey{gateway: c.Gateway, corralUser: c.CorralUser}] = c
	}
	return r, nil
}

// Touch records or updates a client entry and persists the registry to disk.
func (r *ClientRegistry) Touch(gateway, corralUser, hostID string) ([]proto.ClientInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if gateway != "" || corralUser != "" {
		r.clients[clientKey{gateway: gateway, corralUser: corralUser}] = proto.ClientInfo{
			Gateway:    gateway,
			CorralUser: corralUser,
			HostID:     hostID,
			LastSeen:   time.Now().UTC().Format(time.RFC3339),
		}
	}
	list := r.listLocked()
	if err := r.saveLocked(list); err != nil {
		return list, err
	}
	return list, nil
}

// Forget removes the entry for (gateway, corralUser) (and optional hostID when
// specified) and persists the registry.
func (r *ClientRegistry) Forget(gateway, corralUser, hostID string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := clientKey{gateway: gateway, corralUser: corralUser}
	if existing, ok := r.clients[k]; ok {
		if hostID == "" || existing.HostID == hostID {
			delete(r.clients, k)
		}
	}
	list := r.listLocked()
	if err := r.saveLocked(list); err != nil {
		return len(list), err
	}
	return len(list), nil
}

// List returns a deterministic snapshot of all registered clients.
func (r *ClientRegistry) List() []proto.ClientInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listLocked()
}

// Restore merges clients from a hot-update handoff snapshot and persists them.
func (r *ClientRegistry) Restore(clients []proto.ClientInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range clients {
		if c.Gateway == "" && c.CorralUser == "" {
			continue
		}
		k := clientKey{gateway: c.Gateway, corralUser: c.CorralUser}
		if cur, ok := r.clients[k]; !ok || c.LastSeen >= cur.LastSeen {
			r.clients[k] = c
		}
	}
	return r.saveLocked(r.listLocked())
}

func (r *ClientRegistry) listLocked() []proto.ClientInfo {
	out := make([]proto.ClientInfo, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Gateway != out[j].Gateway {
			return out[i].Gateway < out[j].Gateway
		}
		return out[i].CorralUser < out[j].CorralUser
	})
	return out
}

func (r *ClientRegistry) saveLocked(list []proto.ClientInfo) error {
	if r.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileAtomic(r.path, b, 0o600)
}
