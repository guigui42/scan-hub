// Package venstest provides a fake ScanSnap iX500 speaking VENS over
// loopback, modelled on the behaviour recorded in docs/protocol-notes.md.
package venstest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/guigui42/scan-hub/internal/vens"
)

// Options configures a fake scanner.
type Options struct {
	Password         string        // pairing password (identity derived from it)
	HeartbeatTimeout time.Duration // stale session expiry; default 1 s
	ChunkSize        int           // page chunk size; default 256 KiB
	EndOfBatchQuirk  bool          // report 0x2000 + ADF empty after the last sheet (AirScap #1)
	ButtonInterval   time.Duration // spacing of the 3 repeated button datagrams; default 20 ms
	WaitDelay        time.Duration // delay before answering WAIT FOR SCAN (E0)
	// IP is the loopback address the fake binds and sends from; default
	// 127.0.0.1. Other 127/8 addresses work on Linux (macOS needs lo0 aliases).
	IP string
	// HeartbeatACK makes the fake answer each heartbeat with a short datagram
	// to the advertised reply port, as discovery must skip such packets.
	HeartbeatACK bool
	// CloseOnForeignRelease closes the control connection without a reply
	// when RELEASE carries a token that isn't the current session's, as the
	// iX500 did after a power cycle.
	CloseOnForeignRelease bool
}

// Scanner is a running fake.
type Scanner struct {
	opts     Options
	identity string
	ip       net.IP

	udp     *net.UDPConn
	control net.Listener
	data    net.Listener

	mu         sync.Mutex
	sess       *session
	paper      int
	off        bool // powered off: silent on UDP, refuses TCP sessions
	sheetsDone int
	buttonSeq  byte
	stats      Stats
	wg         sync.WaitGroup
}

type session struct {
	token      [8]byte
	clientIP   net.IP
	notifyPort int
	lastHB     time.Time
}

// Stats counts what the fake saw.
type Stats struct {
	Reserves       int
	ReserveCodes   []int32
	Releases       int
	ReleaseActions []uint32
	Forbidden      []string
	Heartbeats     int
	EndScans       int
	PagesServed    int
}

// Start launches the fake on opts.IP (default 127.0.0.1) with ephemeral ports.
func Start(opts Options) (*Scanner, error) {
	if opts.IP == "" {
		opts.IP = "127.0.0.1"
	}
	ip := net.ParseIP(opts.IP).To4()
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("venstest: IP must be an IPv4 loopback address, got %q", opts.IP)
	}
	if opts.HeartbeatTimeout == 0 {
		opts.HeartbeatTimeout = time.Second
	}
	if opts.ChunkSize == 0 {
		opts.ChunkSize = int(vens.PageTransferLen)
	}
	if opts.ButtonInterval == 0 {
		opts.ButtonInterval = 20 * time.Millisecond
	}
	id, err := vens.ComputeIdentity(opts.Password)
	if err != nil {
		return nil, err
	}
	s := &Scanner{opts: opts, identity: id, ip: ip, buttonSeq: 1}
	if s.udp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: ip}); err != nil {
		return nil, err
	}
	if s.control, err = net.Listen("tcp4", net.JoinHostPort(opts.IP, "0")); err != nil {
		s.udp.Close()
		return nil, err
	}
	if s.data, err = net.Listen("tcp4", net.JoinHostPort(opts.IP, "0")); err != nil {
		s.udp.Close()
		s.control.Close()
		return nil, err
	}
	s.wg.Add(3)
	go s.serveUDP()
	go s.serveTCP(s.control, s.handleControl)
	go s.serveTCP(s.data, s.handleData)
	return s, nil
}

// IP is the fake's address.
func (s *Scanner) IP() string { return s.ip.String() }

// DiscoveryPort, ControlPort and DataPort are the fake's listening ports.
func (s *Scanner) DiscoveryPort() int { return s.udp.LocalAddr().(*net.UDPAddr).Port }
func (s *Scanner) ControlPort() int   { return s.control.Addr().(*net.TCPAddr).Port }
func (s *Scanner) DataPort() int      { return s.data.Addr().(*net.TCPAddr).Port }

// Close stops the fake.
func (s *Scanner) Close() {
	s.udp.Close()
	s.control.Close()
	s.data.Close()
	s.wg.Wait()
}

// LoadPaper puts n sheets in the ADF.
func (s *Scanner) LoadPaper(n int) {
	s.mu.Lock()
	s.paper = n
	s.sheetsDone = 0
	s.mu.Unlock()
}

// Stats returns a copy of the counters.
func (s *Scanner) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.ReserveCodes = append([]int32(nil), st.ReserveCodes...)
	st.ReleaseActions = append([]uint32(nil), st.ReleaseActions...)
	st.Forbidden = append([]string(nil), st.Forbidden...)
	return st
}

// Holder returns the token of the live session, if any.
func (s *Scanner) Holder() ([8]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveLocked() {
		return s.sess.token, true
	}
	return [8]byte{}, false
}

// Occupy simulates another client holding the scanner with a live session.
func (s *Scanner) Occupy(tok [8]byte, ip net.IP, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sess = &session{token: tok, clientIP: ip, notifyPort: 1, lastHB: time.Now().Add(d - s.opts.HeartbeatTimeout)}
}

// PressButton sends the 3 repeated event datagrams to the session holder,
// with a per-press sequence counter in the data field (as on the iX500).
func (s *Scanner) PressButton() bool {
	s.mu.Lock()
	if !s.liveLocked() {
		s.mu.Unlock()
		return false
	}
	s.buttonSeq++
	addr := &net.UDPAddr{IP: s.sess.clientIP, Port: s.sess.notifyPort}
	pkt := make([]byte, 48)
	binary.BigEndian.PutUint32(pkt[0:], 48)
	copy(pkt[4:], vens.Magic[:])
	binary.BigEndian.PutUint32(pkt[8:], 1)
	pkt[16] = s.buttonSeq
	s.mu.Unlock()

	// Send from the fake's own address: routers of button events match on it.
	c, err := net.DialUDP("udp4", &net.UDPAddr{IP: s.ip}, addr)
	if err != nil {
		return false
	}
	defer c.Close()
	for i := range 3 {
		if i > 0 {
			time.Sleep(s.opts.ButtonInterval)
		}
		c.Write(pkt)
	}
	return true
}

func (s *Scanner) liveLocked() bool {
	return s.sess != nil && time.Since(s.sess.lastHB) < s.opts.HeartbeatTimeout
}

// --- UDP discovery + heartbeat ---

func (s *Scanner) serveUDP() {
	defer s.wg.Done()
	buf := make([]byte, 512)
	for {
		n, _, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 32 || [4]byte(buf[0:4]) != vens.Magic {
			continue // ssNR companions and noise
		}
		p := buf[:n]
		var tok [8]byte
		copy(tok[:], p[12:20])
		heartbeat := binary.BigEndian.Uint32(p[4:8]) == 1
		clientIP := net.IP(append([]byte(nil), p[8:12]...))
		port := int(binary.BigEndian.Uint16(p[22:24]))

		s.mu.Lock()
		if s.off {
			s.mu.Unlock()
			continue
		}
		live := s.liveLocked()
		if heartbeat {
			s.stats.Heartbeats++
			ack := false
			if s.sess != nil && s.sess.token == tok && live {
				s.sess.lastHB = time.Now()
				ack = s.opts.HeartbeatACK
			}
			s.mu.Unlock()
			if ack {
				s.udp.WriteToUDP(heartbeatACK(), &net.UDPAddr{IP: clientIP, Port: port})
			}
			continue
		}
		// Observed on hardware: no reply to discovery carrying the holder's
		// own token while it is claimed.
		if live && s.sess.token == tok {
			s.mu.Unlock()
			continue
		}
		reply := s.deviceInfoLocked(live)
		s.mu.Unlock()
		s.udp.WriteToUDP(reply, &net.UDPAddr{IP: clientIP, Port: port})
	}
}

func (s *Scanner) deviceInfoLocked(live bool) []byte {
	p := make([]byte, 132)
	copy(p[0:], vens.Magic[:])
	binary.BigEndian.PutUint16(p[8:], 0x0006)
	binary.BigEndian.PutUint16(p[10:], 0x0030)
	binary.BigEndian.PutUint32(p[12:], 0xFFFFFFFF)
	copy(p[16:], s.ip)
	binary.BigEndian.PutUint16(p[22:], uint16(s.DataPort()))
	binary.BigEndian.PutUint16(p[26:], uint16(s.ControlPort()))
	copy(p[28:], []byte{0x00, 0x80, 0x92, 0x00, 0x00, 0x01})
	state := uint32(1)
	if live {
		state = 3
		copy(p[120:], s.sess.clientIP.To4())
	}
	binary.BigEndian.PutUint32(p[36:], state)
	copy(p[40:], "iX500-FAKE000001")
	copy(p[104:], "ScanSnap iX500  ")
	return p
}

// PowerOff simulates closing the paper chute: the session is lost and the
// fake stops answering.
func (s *Scanner) PowerOff() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.off, s.sess = true, nil
}

// PowerOn simulates opening the chute. Like the iX500 on joining Wi-Fi, it
// then broadcasts its power-on announcement (command 0x21) three times to
// UDP announcePort on 127.0.0.1, from its own address; 0 skips that.
func (s *Scanner) PowerOn(announcePort int) {
	s.mu.Lock()
	s.off = false
	s.mu.Unlock()
	if announcePort == 0 {
		return
	}
	c, err := net.DialUDP("udp4", &net.UDPAddr{IP: s.ip}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: announcePort})
	if err != nil {
		return
	}
	defer c.Close()
	p := make([]byte, 48)
	binary.BigEndian.PutUint32(p[0:], 48)
	copy(p[4:], vens.Magic[:])
	binary.BigEndian.PutUint32(p[8:], vens.CmdBroadcast)
	p[16] = 1
	copy(p[20:], s.ip)
	copy(p[24:], []byte{0x00, 0x80, 0x92, 0x00, 0x00, 0x01})
	for range 3 {
		c.Write(p)
	}
}

// heartbeatACK is a short VENS datagram; its exact layout is not decoded by
// clients, which only need to tell it apart from a device-info reply.
func heartbeatACK() []byte {
	p := make([]byte, 32)
	binary.BigEndian.PutUint32(p[0:], 32)
	copy(p[4:], vens.Magic[:])
	return p
}

// --- TCP ---

func (s *Scanner) serveTCP(l net.Listener, h func(net.Conn)) {
	defer s.wg.Done()
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		off := s.off
		s.mu.Unlock()
		if off {
			c.Close()
			continue
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			defer c.Close()
			if _, err := c.Write(resp(16)); err != nil {
				return
			}
			h(c)
		}()
	}
}

func readPacket(c net.Conn) ([]byte, error) {
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var l [4]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(l[:])
	if n < 12 || n > 1<<16 {
		return nil, io.ErrUnexpectedEOF
	}
	p := make([]byte, n)
	copy(p, l[:])
	if _, err := io.ReadFull(c, p[4:]); err != nil {
		return nil, err
	}
	return p, nil
}

func resp(size int) []byte {
	p := make([]byte, size)
	binary.BigEndian.PutUint32(p, uint32(size))
	copy(p[4:], vens.Magic[:])
	return p
}

func (s *Scanner) forbidden(what string) {
	s.mu.Lock()
	s.stats.Forbidden = append(s.stats.Forbidden, what)
	s.mu.Unlock()
}

func (s *Scanner) handleControl(c net.Conn) {
	for {
		p, err := readPacket(c)
		if err != nil || len(p) < 32 {
			return
		}
		var tok [8]byte
		copy(tok[:], p[16:24])
		switch cmd := binary.BigEndian.Uint32(p[8:12]); cmd {
		case vens.CmdReserve:
			if len(p) < 120 {
				return
			}
			ip := net.IP(append([]byte(nil), p[44:48]...))
			port := int(binary.BigEndian.Uint16(p[50:52]))
			id := strings.TrimRight(string(p[52:100]), "\x00")
			s.mu.Lock()
			var code int32
			switch {
			case id != s.identity:
				code = -3
			case s.liveLocked() && s.sess.token != tok:
				code = -4
			default:
				// Same token (adopted) keeps the session; otherwise a new one.
				s.sess = &session{token: tok, clientIP: ip, notifyPort: port, lastHB: time.Now()}
			}
			s.stats.Reserves++
			s.stats.ReserveCodes = append(s.stats.ReserveCodes, code)
			s.mu.Unlock()
			r := resp(20)
			binary.BigEndian.PutUint32(r[8:], uint32(code))
			binary.BigEndian.PutUint32(r[12:], 0x00040000)
			c.Write(r)
		case vens.CmdRelease:
			action := binary.BigEndian.Uint32(p[24:28])
			s.mu.Lock()
			if s.opts.CloseOnForeignRelease && (s.sess == nil || s.sess.token != tok) {
				s.mu.Unlock()
				return
			}
			s.stats.Releases++
			s.stats.ReleaseActions = append(s.stats.ReleaseActions, action)
			if s.sess != nil && s.sess.token == tok {
				s.sess = nil
			}
			s.mu.Unlock()
			c.Write(resp(16))
		case vens.CmdGetWifiStatus:
			r := resp(32)
			binary.BigEndian.PutUint32(r[16:], 3)
			c.Write(r)
		default:
			s.forbidden("control:" + hex8(cmd))
			return
		}
	}
}

func (s *Scanner) handleData(c net.Conn) {
	for {
		p, err := readPacket(c)
		if err != nil || len(p) < 60 {
			return
		}
		cdb := p[48:]
		switch op := cdb[0]; op {
		case vens.SCSIOpcodeInquiry:
			if cdb[1]&1 == 1 && cdb[2] == 0xF0 {
				r := resp(184)
				r[40], r[41] = 0x06, 0xF0
				binary.BigEndian.PutUint16(r[45:], 600)
				binary.BigEndian.PutUint16(r[47:], 600)
				r[49] = 0x11
				binary.BigEndian.PutUint16(r[54:], 50)
				binary.BigEndian.PutUint16(r[56:], 50)
				binary.BigEndian.PutUint16(r[62:], 0x1468)
				binary.BigEndian.PutUint16(r[66:], 0x50E8)
				c.Write(r)
			} else {
				r := resp(136)
				copy(r[48:], "FUJITSU ScanSnap iX500  0U00")
				c.Write(r)
			}
		case 0xD8, 0xD4, 0xEB:
			c.Write(resp(40))
		case 0xD5:
			c.Write(resp(48))
		case 0xD6:
			s.mu.Lock()
			s.stats.EndScans++
			s.mu.Unlock()
			c.Write(resp(40))
		case 0xC2:
			r := resp(72)
			s.mu.Lock()
			if s.paper == 0 {
				binary.BigEndian.PutUint32(r[40:], vens.ADFPaperMask)
				if s.opts.EndOfBatchQuirk && s.sheetsDone > 0 {
					binary.BigEndian.PutUint32(r[44:], uint32(vens.ErrorCodeEndOfBatch))
				}
			}
			s.mu.Unlock()
			c.Write(r)
		case 0xE0:
			time.Sleep(s.opts.WaitDelay)
			r := resp(40)
			s.mu.Lock()
			if s.paper > 0 {
				s.paper--
				s.sheetsDone++
			} else {
				binary.BigEndian.PutUint32(r[12:], 2)
			}
			s.mu.Unlock()
			c.Write(r)
		case vens.SCSIOpcodeRead10:
			if cdb[2] != vens.DataTypeImage {
				s.forbidden("read10:" + hex8(uint32(cdb[2])))
				return
			}
			s.sendChunk(c, int(cdb[10]), cdb[5]&0x80 != 0, int(cdb[11]))
		case vens.SCSIOpcodeRequestSense:
			c.Write(resp(40 + 18))
		default:
			s.forbidden("data:" + hex8(uint32(op)))
			return
		}
	}
}

func (s *Scanner) sendChunk(c net.Conn, sheet int, back bool, chunk int) {
	side := 0
	if back {
		side = 1
	}
	img := PageJPEG(sheet, side)
	start := min(chunk*s.opts.ChunkSize, len(img))
	end := min(start+s.opts.ChunkSize, len(img))
	h := make([]byte, vens.PageHeaderSize)
	binary.BigEndian.PutUint32(h, uint32(vens.PageHeaderSize+end-start))
	copy(h[4:], vens.Magic[:])
	if end == len(img) {
		binary.BigEndian.PutUint32(h[12:], vens.PageTypeFinal)
		s.mu.Lock()
		s.stats.PagesServed++
		s.mu.Unlock()
	}
	h[40], h[41] = byte(sheet), byte(side)
	c.Write(append(h, img[start:end]...))
}

// PageJPEG is the deterministic JPEG the fake serves for (transfer sheet, side).
func PageJPEG(sheet, side int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 120, 170))
	col := color.RGBA{uint8(40 * sheet), uint8(120 * side), 200, 255}
	for y := range 170 {
		for x := range 120 {
			img.Set(x, y, col)
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 80})
	return b.Bytes()
}

func hex8(v uint32) string {
	const digits = "0123456789ABCDEF"
	return "0x" + string(digits[(v>>4)&0xF]) + string(digits[v&0xF])
}
