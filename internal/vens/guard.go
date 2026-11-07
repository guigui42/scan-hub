package vens

// Write-side allowlist (scan-hub, not from AirScap): no code path can put a
// forbidden command on the wire, whatever builds the packet.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// ErrForbidden is returned when a packet is blocked by the guard.
type ErrForbidden struct{ What string }

func (e *ErrForbidden) Error() string { return "vens guard: blocked " + e.What }

// Control channel (TCP 53219): only RESERVE, RELEASE and GET_WIFI_STATUS.
// UPDATE_PSW 0x15, SET_WIFI_MODE 0x31, FIRM_UPDATE 0x51, SET_DEV_INFO 0x14
// and everything else are rejected.
var allowedControl = map[uint32]bool{
	CmdReserve:       true,
	CmdRelease:       true,
	CmdGetWifiStatus: true,
}

// Data channel (TCP 53218): allowed SCSI CDB opcodes at packet offset 48.
var allowedDataOpcodes = map[byte]bool{
	SCSIOpcodeRequestSense: true, // 0x03
	SCSIOpcodeInquiry:      true, // 0x12
	SCSIOpcodeRead10:       true, // 0x28 (image data type only, see below)
	0xC2:                   true, // GET_STATUS
	0xD4:                   true, // SET SCAN CONFIG
	0xD5:                   true, // PREPARE SCAN
	0xD6:                   true, // END SCAN
	0xD8:                   true, // GET SCAN SETTINGS
	0xE0:                   true, // WAIT FOR SCAN
	0xEB:                   true, // CONFIG
}

const dataCDBOffset = 48

// CheckControl validates an outgoing control-channel packet.
func CheckControl(p []byte) error {
	if len(p) < 12 || [4]byte(p[4:8]) != Magic {
		return &ErrForbidden{What: "malformed control packet"}
	}
	cmd := binary.BigEndian.Uint32(p[8:12])
	if !allowedControl[cmd] {
		return &ErrForbidden{What: fmt.Sprintf("control command 0x%02X", cmd)}
	}
	// RELEASE action 1 is ENFORCE (forced release); scan-hub only ever
	// releases its own session normally.
	if cmd == CmdRelease && (len(p) < 28 || binary.BigEndian.Uint32(p[24:28]) != 0) {
		return &ErrForbidden{What: "RELEASE with non-normal action (ENFORCE)"}
	}
	return nil
}

// CheckData validates an outgoing data-channel packet.
func CheckData(p []byte) error {
	if len(p) <= dataCDBOffset+2 || [4]byte(p[4:8]) != Magic {
		return &ErrForbidden{What: "malformed data packet"}
	}
	op := p[dataCDBOffset]
	if !allowedDataOpcodes[op] {
		return &ErrForbidden{What: fmt.Sprintf("data opcode 0x%02X", op)}
	}
	if op == SCSIOpcodeRead10 && p[dataCDBOffset+2] != DataTypeImage {
		// PIXELSIZE (0x80) hangs the firmware mid-session; paper size and
		// carrier sheet reads are not used either.
		return &ErrForbidden{What: fmt.Sprintf("READ(10) data type 0x%02X", p[dataCDBOffset+2])}
	}
	return nil
}

// guardConn checks every Write. Each Write in this package carries exactly
// one complete packet, so per-write inspection is sufficient.
type guardConn struct {
	net.Conn
	check func([]byte) error
	stop  func() bool // unregisters the ctx close hook
}

func (g *guardConn) Close() error {
	if g.stop != nil {
		g.stop()
	}
	return g.Conn.Close()
}

// dial connects, reads the 16-byte welcome under a deadline, and ties the
// connection to ctx: cancelling ctx closes it, unblocking any pending I/O.
func dial(ctx context.Context, host string, port uint16, check func([]byte) error) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	addr := net.JoinHostPort(host, strconv.Itoa(int(port)))
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	welcome := make([]byte, WelcomeSize)
	if _, err := io.ReadFull(conn, welcome); err != nil {
		stop()
		conn.Close()
		return nil, fmt.Errorf("welcome: %w", err)
	}
	if err := ValidateWelcome(welcome); err != nil {
		stop()
		conn.Close()
		return nil, err
	}
	return &guardConn{Conn: conn, check: check, stop: stop}, nil
}

func (g *guardConn) Write(p []byte) (int, error) {
	if err := g.check(p); err != nil {
		return 0, err
	}
	return g.Conn.Write(p)
}
