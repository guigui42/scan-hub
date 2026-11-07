package scansnap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/guigui42/scan-hub/internal/vens"
)

var (
	// ErrHubClosed is returned by Subscribe after Close.
	ErrHubClosed = errors.New("notify hub closed")
	// ErrSubscribed means another manager already listens for that scanner.
	ErrSubscribed = errors.New("scanner already subscribed")
	// ErrNotListening is returned by Subscribe before Listen.
	ErrNotListening = errors.New("notify hub not listening")
)

// Event is what a scanner sent to the hub.
type Event struct {
	// Announce is set for a power-on announcement (UDP 53220); otherwise the
	// event is a Scan-button press with its counter in Seq.
	Announce bool
	Seq      uint32
}

// NotifyHub owns the local UDP ports scanners send unsolicited packets to:
// Scan-button events (55265) and, optionally, power-on announcements
// (53220). It routes them by source IP, so several managers share it and
// several ScanSnaps can use the default ports.
type NotifyHub struct {
	port         int
	announce     bool // also listen for power-on announcements
	announcePort int
	log          *slog.Logger
	wg           sync.WaitGroup

	mu       sync.Mutex
	conn     *net.UDPConn // set by Listen
	aconn    *net.UDPConn // announcements, if listening
	subs     map[netip.Addr]chan Event
	closed   bool
	lastWarn time.Time
}

// NewNotifyHub returns a hub for the button-event port (0 picks a free one);
// nothing is bound until Listen. Call Close once every subscriber has
// stopped.
func NewNotifyHub(port int, log *slog.Logger) *NotifyHub {
	if log == nil {
		log = slog.Default()
	}
	return &NotifyHub{
		port: port,
		log:  log.With("component", "scansnap-notify"),
		subs: map[netip.Addr]chan Event{},
	}
}

// ListenAnnouncements makes Listen also bind port for power-on announcements
// (vens.BroadcastPort on a real network; 0 picks a free one). Call it before
// Listen.
func (h *NotifyHub) ListenAnnouncements(port int) { h.announce, h.announcePort = true, port }

// Listen binds the ports on all IPv4 interfaces and starts routing. Failing to
// bind the announcement port is only logged: scanners are then reclaimed on
// the normal retry schedule.
func (h *NotifyHub) Listen() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		return ErrHubClosed
	case h.conn != nil:
		return errors.New("notify hub already listening")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: h.port})
	if err != nil {
		return fmt.Errorf("listen UDP %d (button events): %w", h.port, err)
	}
	h.conn = conn
	h.wg.Add(1)
	go h.read(conn, false)
	if h.announce {
		if aconn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: h.announcePort}); err != nil {
			h.log.Warn("power-on announcements disabled: a restarted ScanSnap is reclaimed on the retry schedule", "port", h.announcePort, "err", err)
		} else {
			h.aconn = aconn
			h.wg.Add(1)
			go h.read(aconn, true)
		}
	}
	return nil
}

// Port is the bound button-event port, which scanners must be told to send
// events to (the configured one before Listen).
func (h *NotifyHub) Port() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return h.port
	}
	return h.conn.LocalAddr().(*net.UDPAddr).Port
}

// AnnouncePort is the bound announcement port, 0 when not listening for them.
func (h *NotifyHub) AnnouncePort() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.aconn == nil {
		return 0
	}
	return h.aconn.LocalAddr().(*net.UDPAddr).Port
}

// Subscribe delivers the events coming from ip. The hub owns the channel: it
// is closed by unsubscribe (idempotent) or Close.
func (h *NotifyHub) Subscribe(ip string) (<-chan Event, func(), error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return nil, nil, fmt.Errorf("scansnap: scanner IP must be IPv4, got %q", ip)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		return nil, nil, ErrHubClosed
	case h.conn == nil:
		return nil, nil, ErrNotListening
	}
	if _, dup := h.subs[addr]; dup {
		return nil, nil, fmt.Errorf("%w: %s", ErrSubscribed, addr)
	}
	ch := make(chan Event, 8)
	h.subs[addr] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if cur, ok := h.subs[addr]; ok && cur == ch {
				delete(h.subs, addr)
				close(ch)
			}
		})
	}, nil
}

// Close stops routing and closes every remaining subscription.
func (h *NotifyHub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	conn, aconn := h.conn, h.aconn
	h.mu.Unlock()
	var err error
	if conn != nil {
		err = conn.Close()
	}
	if aconn != nil {
		aconn.Close()
	}
	h.wg.Wait()
	h.mu.Lock()
	for addr, ch := range h.subs {
		delete(h.subs, addr)
		close(ch)
	}
	h.mu.Unlock()
	return err
}

func (h *NotifyHub) read(conn *net.UDPConn, announcements bool) {
	defer h.wg.Done()
	buf := make([]byte, 256)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		var ev Event
		if announcements {
			if _, err := vens.ParseBroadcastAdvertisement(buf[:n]); err != nil || n != 48 {
				continue // other VENS traffic on the broadcast port
			}
			ev.Announce = true
		} else {
			seq, ok := parseButton(buf[:n])
			if !ok {
				h.log.Debug("ignored datagram on notify port", "from", from, "bytes", n)
				continue
			}
			ev.Seq = seq
		}
		src := from.Addr().Unmap()
		h.mu.Lock()
		ch, known := h.subs[src]
		switch {
		case known:
			select {
			case ch <- ev:
			default:
				h.log.Warn("scanner event dropped (subscriber busy)", "from", src, "announce", ev.Announce)
			}
		case !ev.Announce && time.Since(h.lastWarn) > time.Minute:
			// Announcements from other ScanSnaps on the LAN are normal.
			h.lastWarn = time.Now()
			h.log.Warn("button event from an unconfigured scanner ignored (check its IP in the scanner list)", "from", src)
		}
		h.mu.Unlock()
	}
}

// parseButton accepts only the exact 48-byte VENS event type 1 and returns
// its per-press counter (data field at offset 16).
func parseButton(p []byte) (uint32, bool) {
	if len(p) != 48 || [4]byte(p[4:8]) != vens.Magic || binary.BigEndian.Uint32(p[8:12]) != 1 {
		return 0, false
	}
	return binary.BigEndian.Uint32(p[16:20]), true
}
