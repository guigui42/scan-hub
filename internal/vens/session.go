// Derived from github.com/mzyy94/AirScap internal/vens @ 177fdae.
// Copyright (c) 2026 mzyy94. MIT License; see THIRD_PARTY_NOTICES.

package vens

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"
)

// ControlSession manages TCP control channel connections (port 53219).
// Each operation opens a new TCP connection, following the scanner protocol.
type ControlSession struct {
	host string
	port uint16
	ctx  context.Context
}

// WithContext returns a copy whose connections close when ctx is done.
func (s *ControlSession) WithContext(ctx context.Context) *ControlSession {
	c := *s
	c.ctx = ctx
	return &c
}

// NewControlSession creates a ControlSession for the given scanner address.
func NewControlSession(host string, port uint16) *ControlSession {
	return &ControlSession{host: host, port: port}
}

// connect opens a TCP connection and reads the welcome packet.
func (s *ControlSession) connect() (net.Conn, error) {
	conn, err := dial(s.ctx, s.host, s.port, CheckControl)
	if err != nil {
		return nil, fmt.Errorf("control connect: %w", err)
	}
	return conn, nil
}

const maxControlResponse = 4096

// sendRecv opens a connection, sends data, reads a length-prefixed response, and closes.
func (s *ControlSession) sendRecv(data []byte) ([]byte, error) {
	conn, err := s.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	slog.Debug("control send", "bytes", len(data), "cmd", fmt.Sprintf("0x%02X", binary.BigEndian.Uint32(data[8:12])))
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("control send: %w", err)
	}

	// Read response length (first 4 bytes)
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, fmt.Errorf("control recv length: %w", err)
	}
	respLen := binary.BigEndian.Uint32(lenBuf)
	if respLen < 4 || respLen > maxControlResponse {
		return nil, fmt.Errorf("control recv: invalid length %d", respLen)
	}

	resp := make([]byte, respLen)
	copy(resp[:4], lenBuf)
	if _, err := io.ReadFull(conn, resp[4:]); err != nil {
		return nil, fmt.Errorf("control recv body: %w", err)
	}
	slog.Debug("control recv", "bytes", len(resp), "hex", hex.EncodeToString(resp))
	return resp, nil
}

// Configure sends client configuration (identity, notify port, etc.) to the scanner.
// Returns true if the scanner accepted the pairing.
func (s *ControlSession) Configure(token [8]byte, clientIP string, notifyPort uint16, identity string) (bool, error) {
	status, _, err := s.ConfigureStatus(token, clientIP, notifyPort, identity)
	if err != nil {
		return false, err
	}
	return status == 0, nil
}

// ConfigureStatus sends RESERVE and returns the signed status (0 accepted,
// -3 bad identity, -4 busy/stale, -7 held by another client) and the raw
// 20-byte response.
func (s *ControlSession) ConfigureStatus(token [8]byte, clientIP string, notifyPort uint16, identity string) (int32, []byte, error) {
	req := MarshalReserveRequest(token, clientIP, notifyPort, identity, time.Now())
	slog.Debug("configuring session", "ip", clientIP, "port", notifyPort, "identity_len", len(identity))

	resp, err := s.sendRecv(req)
	if err != nil {
		return 0, nil, err
	}

	slog.Debug("configure response", "bytes", len(resp))
	status, err := ParseReserveResponse(resp)
	if err != nil {
		return 0, resp, err
	}
	return int32(status), resp, nil
}

// CheckStatus queries the scanner's connection status.
func (s *ControlSession) CheckStatus(token [8]byte) (uint32, error) {
	slog.Debug("checking scanner status...")
	req := MarshalGetWifiStatusRequest(token)
	resp, err := s.sendRecv(req)
	if err != nil {
		return 0, err
	}
	state, err := ParseGetWifiStatusResponse(resp)
	if err != nil {
		return 0, err
	}
	slog.Debug("status check", "state", state, "response_bytes", len(resp))
	return state, nil
}

// Deregister removes this client from the scanner.
func (s *ControlSession) Deregister(token [8]byte) error {
	slog.Debug("deregistering...")
	conn, err := s.connect()
	if err != nil {
		return err
	}
	defer conn.Close()

	req := MarshalReleaseRequest(token, 0)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("deregister send: %w", err)
	}

	ack := make([]byte, 16)
	if _, err := io.ReadFull(conn, ack); err != nil {
		return fmt.Errorf("deregister recv: %w", err)
	}
	if binary.BigEndian.Uint32(ack[0:4]) != 16 || [4]byte(ack[4:8]) != Magic {
		return fmt.Errorf("deregister: unexpected ack %x", ack)
	}
	return nil
}
