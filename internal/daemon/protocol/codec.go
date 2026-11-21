// Package protocol implements the line-delimited JSON-RPC 2.0 wire protocol and codec.
package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// DefaultMaxMessageSize defines the maximum size of a single JSON-RPC frame (1MB).
const DefaultMaxMessageSize = 1024 * 1024

// NewRequest creates a JSON-RPC 2.0 request.
func NewRequest(id string, method string, params any) (*JSONRPCMessage, error) {
	msg := &JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
		msg.Params = raw
	}
	return msg, nil
}

// NewNotification creates a JSON-RPC 2.0 notification without an ID.
func NewNotification(method string, params any) (*JSONRPCMessage, error) {
	msg := &JSONRPCMessage{
		JSONRPC: "2.0",
		Method:  method,
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
		msg.Params = raw
	}
	return msg, nil
}

// NewResponse creates a successful JSON-RPC 2.0 response.
func NewResponse(id string, result any) (*JSONRPCMessage, error) {
	msg := &JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      &id,
	}
	if result != nil {
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		msg.Result = raw
	}
	return msg, nil
}

// NewErrorResponse creates a JSON-RPC 2.0 error response.
func NewErrorResponse(id *string, code int, message string, data any) *JSONRPCMessage {
	errObj := &JSONRPCError{
		Code:    code,
		Message: message,
	}
	if data != nil {
		raw, err := json.Marshal(data)
		if err == nil {
			errObj.Data = raw
		}
	}
	return &JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error:   errObj,
	}
}

// EncodeMessage serializes a JSON-RPC message into bytes terminated by '\n'.
func EncodeMessage(msg *JSONRPCMessage) ([]byte, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to encode json-rpc: %w", err)
	}
	return append(raw, '\n'), nil
}

// DecodeMessage parses a single JSON-RPC message from raw bytes.
func DecodeMessage(data []byte) (*JSONRPCMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("empty message")
	}
	var msg JSONRPCMessage
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	if msg.JSONRPC != "2.0" {
		return nil, errors.New("invalid jsonrpc version, must be '2.0'")
	}
	return &msg, nil
}

// StreamReadError indicates an unrecoverable transport or scanner framing error.
type StreamReadError struct {
	Err error
}

func (e *StreamReadError) Error() string {
	return fmt.Sprintf("stream read error: %v", e.Err)
}

func (e *StreamReadError) Unwrap() error {
	return e.Err
}

// StreamCodec provides line-delimited reading and thread-safe writing of JSON-RPC messages.
type StreamCodec struct {
	scanner *bufio.Scanner
	writer  io.Writer
	mu      sync.Mutex
}

// NewStreamCodec creates a new StreamCodec wrapping reader and writer.
func NewStreamCodec(r io.Reader, w io.Writer) *StreamCodec {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, DefaultMaxMessageSize)
	return &StreamCodec{
		scanner: scanner,
		writer:  w,
	}
}

// ReadMessage blocks until the next newline-delimited message is received.
func (c *StreamCodec) ReadMessage() (*JSONRPCMessage, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, &StreamReadError{Err: err}
		}
		return nil, io.EOF
	}
	return DecodeMessage(c.scanner.Bytes())
}

// WriteMessage serializes and writes a message followed by '\n' atomically.
func (c *StreamCodec) WriteMessage(msg *JSONRPCMessage) error {
	raw, err := EncodeMessage(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.writer.Write(raw)
	return err
}
