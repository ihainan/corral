package proto

import (
	"encoding/json"
	"fmt"
)

// RPC method names.
const (
	MethodDaemonHello    = "daemon.hello"
	MethodDaemonResume   = "daemon.resume"
	MethodDaemonHealth   = "daemon.health"
	MethodDaemonHandoff  = "daemon.handoff"
	MethodDaemonShutdown = "daemon.shutdown"
	MethodDaemonForget   = "daemon.forget"

	// Stage 1 agent methods (implemented by T3).
	MethodAgentLocate       = "agent.locate"
	MethodAgentWatch        = "agent.watch"
	MethodAgentUnwatch      = "agent.unwatch"
	MethodConvoTails        = "convo.tails"
	MethodConvoMessageTails = "convo.message_tails"
	MethodConvoSubscribe    = "convo.subscribe"
	MethodConvoUnsubscribe  = "convo.unsubscribe"
	MethodPaneSend          = "pane.send"
	MethodPaneAnswer        = "pane.answer"
)

// Server-pushed event names.
const (
	EventDaemonReconnect = "daemon.reconnect"
	EventAgentStatus     = "agent.status"
	EventAgentChanged    = "agent.changed"
	EventConvoDelta      = "convo.delta"
)

// Structured RPC error codes.
const (
	ErrProtoMismatch     = "proto_mismatch"
	ErrResyncRequired    = "resync_required"
	ErrHandoffInProgress = "handoff_in_progress"
	ErrHandoffFailed     = "handoff_failed"
	ErrInProgress        = "in_progress"
	ErrInvalidParams     = "invalid_params"
	ErrMethodNotFound    = "method_not_found"
	ErrInternal          = "internal_error"
	ErrRemoving          = "removing"
	ErrUnauthorized      = "unauthorized"
	ErrNotSupported      = "not_supported"
)

// Daemon operational states reported in daemon.hello and status output.
const (
	StateRunning  = "running"
	StateDraining = "draining"
	StateStopped  = "stopped"
)

// Request is a client-to-daemon RPC invocation frame.
type Request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RPCError is the structured error returned inside a Response frame.
type RPCError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Response is a daemon-to-client RPC reply frame.
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Event is an asynchronous server-to-client notification frame.
type Event struct {
	Event string          `json:"event"`
	Epoch string          `json:"epoch"`
	Seq   uint64          `json:"seq"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Envelope can unmarshal any wire frame (Request, Response, or Event) for
// multiplexed stream readers.
type Envelope struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	Epoch  string          `json:"epoch,omitempty"`
	Seq    uint64          `json:"seq,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// IsRequest reports whether the envelope carries an RPC request.
func (e Envelope) IsRequest() bool { return e.Method != "" }

// IsEvent reports whether the envelope carries a pushed event.
func (e Envelope) IsEvent() bool { return e.Event != "" }

// IsResponse reports whether the envelope carries an RPC response.
func (e Envelope) IsResponse() bool { return e.ID != 0 && e.Method == "" && e.Event == "" }

// AsEvent extracts the Event view of the envelope.
func (e Envelope) AsEvent() Event {
	return Event{
		Event: e.Event,
		Epoch: e.Epoch,
		Seq:   e.Seq,
		Data:  e.Data,
	}
}

// AsResponse extracts the Response view of the envelope.
func (e Envelope) AsResponse() Response {
	return Response{
		ID:     e.ID,
		Result: e.Result,
		Error:  e.Error,
	}
}

// ClientIdentity identifies the Corral user and host record on a gateway.
type ClientIdentity struct {
	CorralUser string `json:"corral_user"`
	HostID     string `json:"host_id"`
}

// ClientInfo is a persisted registered client entry returned by daemon.hello.
type ClientInfo struct {
	Gateway    string `json:"gateway"`
	CorralUser string `json:"corral_user"`
	HostID     string `json:"host_id,omitempty"`
	LastSeen   string `json:"last_seen"`
}

// HelloParams are the parameters for daemon.hello.
type HelloParams struct {
	Gateway        string         `json:"gateway"`
	GatewayVersion string         `json:"gateway_version"`
	ProtoMin       int            `json:"proto_min"`
	ProtoMax       int            `json:"proto_max"`
	Client         ClientIdentity `json:"client"`
}

// HelloResult is returned by daemon.hello.
type HelloResult struct {
	Version   string       `json:"version"`
	Release   int64        `json:"release"`
	Commit    string       `json:"commit"`
	Protocol  int          `json:"protocol"`
	Epoch     string       `json:"epoch"`
	Seq       uint64       `json:"seq"`
	PID       int          `json:"pid"`
	StartTime string       `json:"start_time"`
	Manifest  Manifest     `json:"manifest"`
	Clients   []ClientInfo `json:"clients"`
	State     string       `json:"state"`
}

// ResumeParams are the parameters for daemon.resume.
type ResumeParams struct {
	Epoch   string `json:"epoch"`
	LastSeq uint64 `json:"last_seq"`
}

// ResumeResult is returned by daemon.resume when the ring buffer can satisfy
// the requested cursor. Replayed event frames are sent immediately following
// the response in strictly increasing sequence order.
type ResumeResult struct {
	Epoch      string `json:"epoch"`
	Replayed   int    `json:"replayed"`
	CurrentSeq uint64 `json:"current_seq"`
}

// HealthResult is returned by daemon.health.
type HealthResult struct {
	Caps              map[string]string `json:"caps"`
	RSSKB             int64             `json:"rss_kb"`
	UptimeS           int64             `json:"uptime_s"`
	Version           string            `json:"version,omitempty"`
	Release           int64             `json:"release,omitempty"`
	PID               int               `json:"pid,omitempty"`
	State             string            `json:"state,omitempty"`
	Socket            string            `json:"socket,omitempty"`
	ConnectedGateways int               `json:"connected_gateways"`
}

// HandoffParams are the parameters for daemon.handoff.
type HandoffParams struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Release int64  `json:"release"`
}

// HandoffResult is returned by daemon.handoff after the replacement daemon
// signals READY.
type HandoffResult struct {
	PID        int   `json:"pid"`
	Release    int64 `json:"release"`
	DurationMS int64 `json:"duration_ms"`
}

// ShutdownParams are optional parameters for daemon.shutdown.
type ShutdownParams struct {
	Reason string `json:"reason,omitempty"`
}

// ShutdownResult is returned by daemon.shutdown.
type ShutdownResult struct {
	OK bool `json:"ok"`
}

// ForgetParams are the parameters for daemon.forget.
type ForgetParams struct {
	Gateway    string `json:"gateway"`
	CorralUser string `json:"corral_user"`
	HostID     string `json:"host_id,omitempty"`
}

// ForgetResult is returned by daemon.forget.
type ForgetResult struct {
	RemainingClients int `json:"remaining_clients"`
}

// ReconnectEventData is the payload of daemon.reconnect events sent before an
// old daemon exits after a successful handoff.
type ReconnectEventData struct {
	Reason     string `json:"reason"`
	Seq        uint64 `json:"seq"`
	NewPID     int    `json:"new_pid,omitempty"`
	NewRelease int64  `json:"new_release,omitempty"`
}

// AgentLocateParams are the parameters for agent.locate, mirroring the five
// positional arguments of convoLocateWith plus optional structured fast-path
// fields (ShellPID + PIDs).
type AgentLocateParams struct {
	Mode     string `json:"mode,omitempty"`
	TmuxName string `json:"tmux_name,omitempty"`
	TmuxPane string `json:"tmux_pane,omitempty"`
	Times    bool   `json:"times,omitempty"`
	Only     string `json:"only,omitempty"`
	ShellPID int    `json:"shell_pid,omitempty"`
	PIDs     []int  `json:"pids,omitempty"`
}

// AgentLocateResult is returned by agent.locate.
// Out is encoded as base64 in JSON ([]byte) so NUL record separators and any
// non-UTF-8 bytes in paths/titles survive wire framing verbatim.
type AgentLocateResult struct {
	Out []byte `json:"out"`
}

// ConvoTailsParams are the parameters for convo.tails and convo.message_tails.
type ConvoTailsParams struct {
	Logs []string `json:"logs"`
}

// ConvoTailsResult is returned by convo.tails and convo.message_tails.
// Out is base64-encoded ([]byte) so NUL delimiters, \x01 error markers, and
// byte-bounded tail slices that may split multi-byte UTF-8 sequences are
// preserved byte-for-byte.
type ConvoTailsResult struct {
	Out []byte `json:"out"`
}

// AgentWatchParams are the parameters for agent.watch.
type AgentWatchParams struct {
	Stop bool     `json:"stop,omitempty"`
	Logs []string `json:"logs,omitempty"`
}

// AgentWatchResult is returned by agent.watch and agent.unwatch.
type AgentWatchResult struct {
	Watching     bool   `json:"watching"`
	Mode         string `json:"mode"` // "inotify", "poll", or "idle"
	WatchedFiles int    `json:"watched_files"`
	MaxFiles     int    `json:"max_files"`
	Epoch        string `json:"epoch"`
	Seq          uint64 `json:"seq"`
}

// AgentChangedEventData is the hint-only payload of agent.changed events.
// It carries only PIDs and log paths that changed, never conversation or
// terminal content.
type AgentChangedEventData struct {
	AddedPIDs   []int    `json:"added_pids,omitempty"`
	RemovedPIDs []int    `json:"removed_pids,omitempty"`
	ChangedLogs []string `json:"changed_logs,omitempty"`
}
