package a2a

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestJSONRPCError_Error(t *testing.T) {
	e := NewError(ErrTaskNotFound, "Task not found", "task-123")
	got := e.Error()
	want := "jsonrpc -32001: Task not found"
	if got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	var nilErr *JSONRPCError
	if nilErr.Error() != "" {
		t.Fatalf("nil Error() should be empty string")
	}
}

func TestTaskState_IsTerminal(t *testing.T) {
	cases := map[TaskState]bool{
		TaskStateSubmitted:     false,
		TaskStateWorking:       false,
		TaskStateInputRequired: false,
		TaskStateCompleted:     true,
		TaskStateCanceled:      true,
		TaskStateFailed:        true,
	}
	for state, want := range cases {
		if got := state.IsTerminal(); got != want {
			t.Errorf("state %q IsTerminal = %v, want %v", state, got, want)
		}
	}
}

func TestMessage_FirstText(t *testing.T) {
	m := Message{Parts: []Part{{}, {Text: "hello"}, {Text: "world"}}}
	if got := m.FirstText(); got != "hello" {
		t.Errorf("FirstText = %q, want %q", got, "hello")
	}
	empty := Message{Parts: []Part{{}, {}}}
	if got := empty.FirstText(); got != "" {
		t.Errorf("empty FirstText = %q, want empty", got)
	}
}

func TestPart_UnmarshalDataPart_PreservesData(t *testing.T) {
	// A data-typed Part must retain its `data` payload — the hub forwards
	// this verbatim to upstreams like OmniLauncher that expect it.
	in := `{"type":"data","data":{"query":"hi","tags":["a","b"]}}`
	var p Part
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if p.Type != "data" {
		t.Errorf("Type = %q, want data", p.Type)
	}
	if len(p.Data) == 0 {
		t.Fatalf("Data is empty; expected raw JSON of the data object")
	}
	var data map[string]any
	if err := json.Unmarshal(p.Data, &data); err != nil {
		t.Fatalf("decode Data: %v", err)
	}
	if data["query"] != "hi" {
		t.Errorf("data.query = %v, want hi", data["query"])
	}
}

func TestPart_RoundTrip_DataPart(t *testing.T) {
	// A data Part must survive Marshal(Unmarshal(x)) — this is the exact
	// path the dispatcher takes when forwarding to an upstream.
	in := `{"data":{"query":"hi"},"type":"data"}`
	var p Part
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Compare structurally to avoid key-order flakiness.
	var got, want map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if err := json.Unmarshal([]byte(in), &want); err != nil {
		t.Fatalf("baseline unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got=%v\nwant=%v", got, want)
	}
}

func TestPart_RoundTrip_PreservesUnknownFields(t *testing.T) {
	// The hub is a forwarding hop; unknown Part fields (future variants,
	// upstream-specific metadata) must survive the trip.
	in := `{"type":"file","file":{"name":"x.pdf","bytes":"AAAA"},"metadata":{"trace":"abc"}}`
	var p Part
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if p.Type != "file" {
		t.Errorf("Type = %q, want file", p.Type)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(out, &got)
	_ = json.Unmarshal([]byte(in), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown fields lost:\n got=%v\nwant=%v", got, want)
	}
}

func TestPart_MarshalTextOnly_ProducesTextShape(t *testing.T) {
	// The common construction `Part{Text: "hi"}` must still serialize
	// exactly as before: {"text":"hi"}, no data / no type when unset.
	b, err := json.Marshal(Part{Text: "hi"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `{"text":"hi"}` {
		t.Fatalf("unexpected encoding: %s", string(b))
	}
}

func TestPart_ExtraCannotShadowTypedFields(t *testing.T) {
	// Defensive: even if a caller stuffs "type"/"text"/"data" into Extra,
	// the typed fields win on marshal.
	p := Part{
		Type: "text",
		Text: "real",
		Extra: map[string]json.RawMessage{
			"type": json.RawMessage(`"file"`),
			"text": json.RawMessage(`"fake"`),
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["type"] != "text" || got["text"] != "real" {
		t.Fatalf("typed fields lost precedence over Extra: %v", got)
	}
}

func TestFirstText_IgnoresDataPart(t *testing.T) {
	// FirstText scans for the first non-empty .Text; data-only parts must
	// not stall the scan.
	m := Message{Parts: []Part{
		{Type: "data", Data: json.RawMessage(`{"q":"hi"}`)},
		{Type: "text", Text: "hello"},
	}}
	if got := m.FirstText(); got != "hello" {
		t.Errorf("FirstText = %q, want hello", got)
	}
}

func TestJSONRPCRequestRoundTrip(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":42,"method":"message/send","params":{"skillId":"foo"}}`
	var req JSONRPCRequest
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.Method != "message/send" {
		t.Errorf("method = %q, want message/send", req.Method)
	}
	if string(req.ID) != "42" {
		t.Errorf("id = %q, want 42", string(req.ID))
	}
	var params SendMessageParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("params unmarshal: %v", err)
	}
	if params.SkillID != "foo" {
		t.Errorf("skillId = %q, want foo", params.SkillID)
	}
}
