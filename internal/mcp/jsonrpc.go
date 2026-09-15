package mcp

import (
	"encoding/json"
	"fmt"
)

// Request is a JSON-RPC 2.0 request or notification (ID nil => notification).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	// Method is set when the peer sent us a request or notification rather
	// than a response.
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("rpc error %d: %s: %s", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// IsNotification reports whether the message is a peer-initiated notification.
func (r *Response) IsNotification() bool { return r.Method != "" && len(r.ID) == 0 }

// IsPeerRequest reports whether the message is a peer-initiated request that
// expects a reply (e.g. sampling/createMessage, roots/list, elicitation).
func (r *Response) IsPeerRequest() bool { return r.Method != "" && len(r.ID) > 0 }

// RPCDetails exposes the JSON-RPC error code and data for output formatting.
func (e *RPCError) RPCDetails() (int, json.RawMessage) { return e.Code, e.Data }
