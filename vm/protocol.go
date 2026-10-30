package vm

import "encoding/json"

// Protocol message types — first byte on the Unix socket connection.
const (
	// MsgConsoleAttach requests a bidirectional PTY relay.
	// After the daemon sends an ACK byte (MsgConsoleAttach), raw bytes
	// flow in both directions until the client disconnects.
	MsgConsoleAttach byte = 0x01

	// MsgControl sends a JSON-encoded control command.
	// Format: [0x02] [4-byte big-endian length] [JSON request]
	// Response: [0x02] [4-byte big-endian length] [JSON response]
	MsgControl byte = 0x02
)

// Control command names.
const (
	CmdPause   = "pause"
	CmdResume  = "resume"
	CmdSuspend = "suspend"
	CmdStop    = "stop"
	CmdStatus  = "status"
	CmdResize  = "resize"
)

// ControlRequest is a JSON-encoded command sent from client to daemon.
type ControlRequest struct {
	Cmd   string `json:"cmd"`
	Force bool   `json:"force,omitempty"` // for stop: forceful termination
	Rows  uint16 `json:"rows,omitempty"`  // for resize: terminal rows
	Cols  uint16 `json:"cols,omitempty"`  // for resize: terminal columns
}

// ControlResponse is a JSON-encoded reply from daemon to client.
type ControlResponse struct {
	OK     bool   `json:"ok"`
	Status string `json:"status,omitempty"` // current VM status after the operation
	Forced bool   `json:"forced,omitempty"` // stop: the guest ignored the request and was force-stopped
	Error  string `json:"error,omitempty"`
}

// MarshalControlRequest encodes a control request to JSON bytes.
func MarshalControlRequest(req ControlRequest) ([]byte, error) {
	return json.Marshal(req)
}

// UnmarshalControlRequest decodes a control request from JSON bytes.
func UnmarshalControlRequest(data []byte) (ControlRequest, error) {
	var req ControlRequest
	err := json.Unmarshal(data, &req)
	return req, err
}

// MarshalControlResponse encodes a control response to JSON bytes.
func MarshalControlResponse(resp ControlResponse) ([]byte, error) {
	return json.Marshal(resp)
}

// UnmarshalControlResponse decodes a control response from JSON bytes.
func UnmarshalControlResponse(data []byte) (ControlResponse, error) {
	var resp ControlResponse
	err := json.Unmarshal(data, &resp)
	return resp, err
}
