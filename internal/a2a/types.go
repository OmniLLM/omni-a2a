// Package a2a defines the A2A (Agent-to-Agent) protocol types and JSON-RPC helpers.
// Based on the A2A specification for JSON-RPC communication between agents.
//
// This package is pure types and pure JSON encoding/decoding helpers. It has no
// dependencies on any other internal package and no side effects (no logging,
// no I/O). Every other package builds on it.
package a2a

import (
	"encoding/json"
	"fmt"
)

// --- Agent card ---

// AgentCard describes an agent's capabilities and metadata for discovery.
type AgentCard struct {
	Name                string            `json:"name"`
	Description         string            `json:"description"`
	URL                 string            `json:"url"`
	Version             string            `json:"version"`
	SupportedInterfaces []AgentInterface  `json:"supportedInterfaces,omitempty"`
	Capabilities        AgentCapabilities `json:"capabilities"`
	Authentication      *AgentAuth        `json:"authentication,omitempty"`
	DefaultInputModes   []string          `json:"defaultInputModes"`
	DefaultOutputModes  []string          `json:"defaultOutputModes"`
	Skills              []AgentSkill      `json:"skills"`
}

// AgentAuth describes accepted auth schemes.
type AgentAuth struct {
	Schemes []string `json:"schemes"`
}

// AgentInterface defines how to reach the agent.
type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
}

// AgentCapabilities lists what the agent supports.
type AgentCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
}

// AgentSkill describes one capability of the agent.
type AgentSkill struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	Capabilities *AgentCapabilities `json:"capabilities,omitempty"`
}

// --- JSON-RPC types ---

// JSONRPCRequest represents an incoming JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse represents an outgoing JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError is a JSON-RPC error object.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *JSONRPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// Standard JSON-RPC error codes.
const (
	ErrParseError      = -32700
	ErrInvalidRequest  = -32600
	ErrMethodNotFound  = -32601
	ErrInvalidParams   = -32602
	ErrInternal        = -32603
	ErrTaskNotFound    = -32001
	ErrUpstreamHTTP    = -32002
	ErrInvalidUpstream = -32003
	ErrUnavailable     = -32010
	ErrNoRoute         = -32011
	ErrGeneric         = -32000
)

// NewError constructs a JSON-RPC error with the given code, message, and data payload.
func NewError(code int, message string, data any) *JSONRPCError {
	return &JSONRPCError{Code: code, Message: message, Data: data}
}

// --- Message types ---

// Role represents who sent a message.
type Role string

const (
	RoleUser  Role = "user"
	RoleAgent Role = "agent"
)

// Part represents a piece of content in a message.
//
// A2A defines several Part variants (text, data, and — over time — others such
// as `file` or `image`). The hub is a forwarding node, not the terminal
// consumer, so we don't want to lose fields we don't yet understand. Part
// therefore uses a custom (Un)Marshal pair that keeps the well-known fields
// (`type`, `text`, `data`) as typed fields and preserves every other JSON key
// verbatim in Extra. Extra is emitted back exactly as received on marshal.
//
// Adding a field here (or letting Extra carry it) is additive by design:
// existing callers that only set Text continue to work unchanged.
type Part struct {
	Type string          `json:"-"`
	Text string          `json:"-"`
	Data json.RawMessage `json:"-"`
	// Extra holds JSON keys other than type/text/data so the hub round-trips
	// unknown Part variants losslessly. Nil map == no extras.
	Extra map[string]json.RawMessage `json:"-"`
}

// MarshalJSON emits the well-known fields (omitting empty ones) plus any
// extras, exactly as they were received.
func (p Part) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, 3+len(p.Extra))
	for k, v := range p.Extra {
		// Defensive: never let Extra shadow the typed fields.
		if k == "type" || k == "text" || k == "data" {
			continue
		}
		out[k] = v
	}
	if p.Type != "" {
		b, err := json.Marshal(p.Type)
		if err != nil {
			return nil, err
		}
		out["type"] = b
	}
	if p.Text != "" {
		b, err := json.Marshal(p.Text)
		if err != nil {
			return nil, err
		}
		out["text"] = b
	}
	if len(p.Data) > 0 {
		out["data"] = p.Data
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes a Part while preserving every JSON key. Unknown keys
// land in Extra and are re-emitted on MarshalJSON — this matters because the
// hub forwards Parts to upstreams that may recognize variants (e.g. file,
// image) the hub itself doesn't model.
func (p *Part) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*p = Part{}
	for k, v := range raw {
		switch k {
		case "type":
			if err := json.Unmarshal(v, &p.Type); err != nil {
				return fmt.Errorf("part.type: %w", err)
			}
		case "text":
			if err := json.Unmarshal(v, &p.Text); err != nil {
				return fmt.Errorf("part.text: %w", err)
			}
		case "data":
			// Retain the raw bytes so structured payloads round-trip byte-
			// for-byte (modulo whitespace) to the upstream.
			p.Data = append(p.Data[:0], v...)
		default:
			if p.Extra == nil {
				p.Extra = make(map[string]json.RawMessage, len(raw))
			}
			p.Extra[k] = v
		}
	}
	return nil
}

// Message is a user or agent message in A2A.
type Message struct {
	MessageID string `json:"messageId"`
	Role      Role   `json:"role"`
	Parts     []Part `json:"parts"`
}

// FirstText returns the first non-empty text Part in the message, or "".
func (m Message) FirstText() string {
	for _, p := range m.Parts {
		if p.Text != "" {
			return p.Text
		}
	}
	return ""
}

// --- Task types ---

// TaskState describes the lifecycle state of a task.
type TaskState string

const (
	TaskStateSubmitted     TaskState = "submitted"
	TaskStateWorking       TaskState = "working"
	TaskStateInputRequired TaskState = "input-required"
	TaskStateCompleted     TaskState = "completed"
	TaskStateCanceled      TaskState = "canceled"
	TaskStateFailed        TaskState = "failed"
)

// IsTerminal reports whether the state cannot transition to another state.
func (s TaskState) IsTerminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateCanceled, TaskStateFailed:
		return true
	}
	return false
}

// TaskStatus wraps a task state with optional messages.
type TaskStatus struct {
	State   TaskState `json:"state"`
	Message *Message  `json:"message,omitempty"`
}

// Artifact is a piece of output from a task.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
	Parts      []Part `json:"parts"`
}

// Task represents a unit of work in the A2A protocol.
type Task struct {
	TaskID    string     `json:"id"`
	ContextID string     `json:"contextId"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	History   []Message  `json:"history,omitempty"`
}

// --- SSE stream event types ---

// TaskStatusUpdateEvent is emitted on the SSE stream when a task's state changes.
type TaskStatusUpdateEvent struct {
	TaskID    string     `json:"id"`
	ContextID string     `json:"contextId,omitempty"`
	Status    TaskStatus `json:"status"`
	Final     bool       `json:"final,omitempty"`
}

// TaskArtifactUpdateEvent is emitted on the SSE stream when a task produces an artifact chunk.
type TaskArtifactUpdateEvent struct {
	TaskID    string   `json:"id"`
	ContextID string   `json:"contextId,omitempty"`
	Artifact  Artifact `json:"artifact"`
}

// --- Method-specific parameter types ---

// SendMessageParams are the params for message/send and message/sendSubscribe.
type SendMessageParams struct {
	Message   Message `json:"message"`
	ContextID string  `json:"contextId,omitempty"`
	SkillID   string  `json:"skillId,omitempty"`
}

// GetTaskParams are the params for tasks/get.
type GetTaskParams struct {
	TaskID string `json:"id"`
}

// CancelTaskParams are the params for tasks/cancel.
type CancelTaskParams struct {
	TaskID string `json:"id"`
}
