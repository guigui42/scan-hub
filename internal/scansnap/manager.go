// Package scansnap owns the ScanSnap iX500 over Wi-Fi: one actor goroutine
// serializes claim, heartbeat, liveness checks, scans and shutdown.
// Behaviour follows docs/protocol-notes.md.
package scansnap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guigui42/scan-hub/internal/vens"
)

// State is the ownership state shown to users.
type State string

const (
	StateConnecting   State = "connecting"
	StateClaimed      State = "claimed"
	StateScanning     State = "scanning"
	StateHeldByOther  State = "held-by-other"
	StateBadPassword  State = "bad-password"
	StateStopping     State = "stopping"
	StateStopped      State = "stopped"
	StateNotConnected State = "not-connected"
	// StateReleased: released on request so another computer can use the
	// scanner; scan-hub doesn't claim it until TakeBack.
	StateReleased State = "released"
)

var (
	// ErrBusy means a scan is already running.
	ErrBusy = errors.New("scanner busy")
	// ErrNotReady means scan-hub does not currently hold the scanner.
	ErrNotReady = errors.New("scanner not claimed")
)

// Config configures a Manager. Zero ports and durations take defaults.
type Config struct {
	ScannerIP   string
	Password    string
	JournalPath string

	ControlPort        int // scanner TCP, default 53219
	DataPort           int // scanner TCP, default 53218
	DiscoveryPort      int // scanner UDP, default 52217
	DiscoveryReplyPort int // local UDP, default 55264
	NotifyPort         int // local UDP for button events, default 55265 (ignored with Hub)

	// Hub shares the button-event port with other managers. Nil gives the
	// manager a private listener on NotifyPort.
	Hub *NotifyHub

	HeartbeatInterval time.Duration // default 500 ms
	StatusInterval    time.Duration // liveness poll while idle, default 10 s
	RetryMin          time.Duration // default 2 s
	RetryMax          time.Duration // default 60 s
	BadPasswordRetry  time.Duration // default 5 min
	ScanGrace         time.Duration // on shutdown, time a running scan may finish; default 20 s
	ButtonDebounce    time.Duration // same counter within this window is a repeat; default 5 s
	AnnounceDebounce  time.Duration // power-on announcements within this window count once; default 10 s

	Logger *slog.Logger
}

func (c *Config) defaults() {
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	defd := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	def(&c.ControlPort, vens.DefaultControlPort)
	def(&c.DataPort, vens.DefaultDataPort)
	def(&c.DiscoveryPort, vens.DiscoveryPort)
	def(&c.DiscoveryReplyPort, vens.ClientDiscoveryPort)
	if c.Hub == nil {
		def(&c.NotifyPort, vens.ClientNotifyPort)
	}
	defd(&c.HeartbeatInterval, 500*time.Millisecond)
	defd(&c.StatusInterval, 10*time.Second)
	defd(&c.RetryMin, 2*time.Second)
	defd(&c.RetryMax, time.Minute)
	defd(&c.BadPasswordRetry, 5*time.Minute)
	defd(&c.ScanGrace, 20*time.Second)
	defd(&c.ButtonDebounce, 5*time.Second)
	defd(&c.AnnounceDebounce, 10*time.Second)
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Status is a snapshot for the UI and /status.
type Status struct {
	State       State     `json:"state"`
	Since       time.Time `json:"since"`
	Detail      string    `json:"detail,omitempty"`
	Holder      string    `json:"holder,omitempty"`
	DeviceName  string    `json:"device_name,omitempty"`
	Firmware    string    `json:"firmware,omitempty"`
	Paper       bool      `json:"paper"`
	CoverOpen   bool      `json:"cover_open"`
	Jam         bool      `json:"jam"`
	WifiSignal  int       `json:"wifi_signal"`
	LastChecked time.Time `json:"last_checked,omitzero"`
}

// ButtonEvent is one debounced press of the scanner's Scan button.
type ButtonEvent struct {
	At  time.Time
	Seq uint32
}

// Page is one scanned side.
type Page struct {
	Sheet int    // physical sheet, 0-based
	Side  int    // 0 front, 1 back
	Data  []byte // JPEG (colour/grey) or TIFF (B&W)
}

// ScanRequest describes one ADF batch.
type ScanRequest struct {
	Config vens.ScanConfig
	// OnPage is called on the actor goroutine for each non-empty page, in
	// order. Returning an error stops the batch.
	OnPage func(Page) error
}

// ScanResult reports a finished batch. Err is set when the batch stopped
// early; pages already delivered remain valid.
type ScanResult struct {
	Pages int
	Err   error
}

type scanJob struct {
	ctx  context.Context
	req  ScanRequest
	resp chan ScanResult
}

// Manager owns the scanner. Create with New, then call Run.
type Manager struct {
	cfg     Config
	log     *slog.Logger
	jr      journal
	jobs    chan scanJob
	buttons chan ButtonEvent
	wake    chan struct{} // the scanner announced it just started
	ctl     chan ctlReq   // release / take back

	mu     sync.RWMutex
	status Status
	ran    atomic.Bool

	// Actor-owned state (only touched by Run's goroutine).
	token       [8]byte
	claimed     bool
	hb          *vens.Heartbeat
	control     *vens.ControlSession
	controlPort int
	dataPort    int
	pollFails   int
	backoff     time.Duration
	notifyPort  int
	lastWake    time.Time
	released    bool
}

type ctlReq struct {
	release bool
	resp    chan error
}

// New returns a Manager; nothing touches the network until Run.
func New(cfg Config) *Manager {
	cfg.defaults()
	m := &Manager{
		cfg:         cfg,
		log:         cfg.Logger.With("component", "scansnap", "ip", cfg.ScannerIP),
		jr:          journal{path: cfg.JournalPath, ip: cfg.ScannerIP},
		jobs:        make(chan scanJob),
		buttons:     make(chan ButtonEvent, 4),
		wake:        make(chan struct{}, 1),
		ctl:         make(chan ctlReq),
		controlPort: cfg.ControlPort,
		dataPort:    cfg.DataPort,
	}
	m.status = Status{State: StateNotConnected, Since: time.Now()}
	return m
}

// Status returns the current snapshot.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Buttons delivers debounced Scan-button presses. It is closed when Run
// returns.
func (m *Manager) Buttons() <-chan ButtonEvent { return m.buttons }

// Scan runs one batch on the actor. It returns ErrBusy while another scan
// runs and ErrNotReady when the scanner is not claimed. Cancelling ctx closes
// the scan socket.
func (m *Manager) Scan(ctx context.Context, req ScanRequest) ScanResult {
	switch m.Status().State {
	case StateScanning:
		return ScanResult{Err: ErrBusy}
	case StateClaimed:
	default:
		return ScanResult{Err: ErrNotReady}
	}
	job := scanJob{ctx: ctx, req: req, resp: make(chan ScanResult, 1)}
	select {
	case m.jobs <- job:
	case <-time.After(2 * time.Second):
		return ScanResult{Err: ErrBusy}
	case <-ctx.Done():
		return ScanResult{Err: ctx.Err()}
	}
	return <-job.resp
}

func (m *Manager) setStatus(f func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.status.State
	f(&m.status)
	if m.status.State != prev {
		m.status.Since = time.Now()
		m.log.Info("state", "from", prev, "to", m.status.State, "detail", m.status.Detail, "holder", m.status.Holder)
	}
}

func (m *Manager) setState(s State, detail string) {
	m.setStatus(func(st *Status) {
		st.State, st.Detail = s, detail
		if s != StateHeldByOther {
			st.Holder = ""
		}
	})
}

// Run owns the scanner until ctx is cancelled, then shuts down in order:
// finish or abort the scan (D6 when possible), RELEASE, stop heartbeat,
// mark the journal clean. A Manager runs once.
func (m *Manager) Run(ctx context.Context) error {
	if !m.ran.CompareAndSwap(false, true) {
		return errors.New("scansnap: manager already ran")
	}
	defer close(m.buttons)
	stopButtons, err := m.listenButtons(ctx)
	if err != nil {
		return err
	}
	defer stopButtons()

	m.backoff = m.cfg.RetryMin
	retry := time.After(0)
	if st, err := m.jr.load(); err == nil && st.Released {
		// Released before the last stop: stay off the scanner.
		m.released, retry = true, nil
		m.setState(StateReleased, "")
	}
	tick := time.NewTicker(m.cfg.StatusInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return nil
		case <-retry:
			retry = nil
			if d := m.claim(ctx); !m.claimed {
				retry = time.After(d)
			}
		case <-tick.C:
			if m.claimed && !m.poll(ctx) {
				retry = time.After(m.cfg.RetryMin)
			}
		case job := <-m.jobs:
			m.runScan(ctx, job)
		case req := <-m.ctl:
			if req.release {
				req.resp <- m.release(ctx)
				retry = nil
			} else {
				changed, err := m.takeBack()
				req.resp <- err
				if changed {
					m.backoff = m.cfg.RetryMin
					retry = time.After(0)
				}
			}
		case <-m.wake:
			if m.released {
				continue
			}
			if time.Since(m.lastWake) < m.cfg.AnnounceDebounce {
				continue // the rest of the same burst
			}
			m.lastWake = time.Now()
			if m.claimed {
				// It restarted under us (e.g. chute closed and reopened between
				// two polls): our session is gone, reclaim with the same token.
				m.log.Info("scanner announced a restart; reclaiming")
				m.stopHeartbeat()
				m.claimed = false
			} else {
				m.log.Info("scanner announced itself; reconnecting now")
			}
			m.setState(StateConnecting, "scanner just started")
			m.backoff = m.cfg.RetryMin
			retry = time.After(0)
		}
	}
}

// nextBackoff doubles the retry delay, with up to 25% jitter so several
// ScanSnaps that went offline together don't queue for discovery in lockstep.
func (m *Manager) nextBackoff() time.Duration {
	d := m.backoff
	m.backoff = min(m.backoff*2, m.cfg.RetryMax)
	return d + rand.N(d/4+1)
}

// claim tries once to take the scanner and returns the delay before the next
// attempt when it fails.
func (m *Manager) claim(ctx context.Context) time.Duration {
	st, err := m.jr.load()
	if err != nil {
		m.setState(StateNotConnected, "journal unreadable: "+err.Error())
		return m.nextBackoff()
	}
	if st.Dirty && !st.belongsTo(m.cfg.ScannerIP) {
		// Its token proves a session on another scanner: never reuse it here.
		m.setState(StateNotConnected, "journal records a session of the scanner at "+st.ScannerIP)
		return m.cfg.RetryMax
	}
	tok, adopted := st.token()
	adopted = adopted && st.Dirty
	if !adopted {
		tok = vens.NewToken()
	}
	if err := m.jr.begin(tok); err != nil {
		m.setState(StateNotConnected, "journal write failed: "+err.Error())
		return m.nextBackoff()
	}
	m.token = tok
	m.setState(StateConnecting, "")
	log := m.log.With("adopted", adopted)

	// Best effort: lets the scanner learn the token and reports its ports.
	if info, err := vens.FindScanner(ctx, m.discoveryOpts(tok, 3*time.Second)); err == nil {
		if info.ControlPort != 0 {
			m.controlPort = int(info.ControlPort)
		}
		if info.DataPort != 0 {
			m.dataPort = int(info.DataPort)
		}
	} else if ctx.Err() == nil {
		log.Debug("discovery with session token got no reply (continuing)", "err", err)
	}
	m.control = vens.NewControlSession(m.cfg.ScannerIP, uint16(m.controlPort))
	ctl := m.control.WithContext(ctx)

	identity, err := vens.ComputeIdentity(m.cfg.Password)
	if err != nil {
		if err := m.jr.clean(); err != nil {
			m.setState(StateNotConnected, "journal write failed: "+err.Error())
			return m.nextBackoff()
		}
		m.setState(StateBadPassword, err.Error())
		return m.cfg.BadPasswordRetry
	}
	if err := m.startHeartbeat(ctx); err != nil {
		m.setState(StateNotConnected, "heartbeat: "+err.Error())
		return m.nextBackoff()
	}
	if !sleepCtx(ctx, 300*time.Millisecond) {
		m.stopHeartbeat()
		return 0
	}

	localIP := vens.GetLocalIP(m.cfg.ScannerIP)
	code, _, err := ctl.ConfigureStatus(tok, localIP, uint16(m.notifyPort), identity)
	if err != nil {
		// The scanner may have processed the RESERVE: keep the journal dirty
		// so the next attempt reuses this token.
		m.stopHeartbeat()
		m.setState(StateNotConnected, "unreachable: "+err.Error())
		return m.nextBackoff()
	}
	log.Info("RESERVE", "status", code)

	switch code {
	case 0:
		m.claimed = true
		m.pollFails = 0
		m.backoff = m.cfg.RetryMin
		m.setup(ctx)
		if m.claimed {
			m.setState(StateClaimed, "")
		}
		return m.cfg.RetryMin
	case -3:
		m.stopHeartbeat()
		if err := m.jr.clean(); err != nil {
			m.setState(StateNotConnected, "journal write failed: "+err.Error())
			return m.nextBackoff()
		}
		m.setState(StateBadPassword, "scanner rejected the password (-3)")
		return m.cfg.BadPasswordRetry
	case -4:
		m.stopHeartbeat()
		if adopted {
			// Maybe our own stale session: normal RELEASE with its token. If
			// the scanner refuses it (seen after a power cycle: the connection
			// is closed), the session isn't ours to prove: treat it as held
			// by someone else. A stale session of ours would expire on its own
			// about 20 s after our last heartbeat.
			if err := ctl.Deregister(tok); err != nil {
				log.Warn("release with the journal token refused; treating the scanner as held by another client", "err", err)
				break
			}
			if err := m.jr.clean(); err != nil {
				m.setState(StateNotConnected, "journal write failed: "+err.Error())
				return m.nextBackoff()
			}
			log.Info("released own stale session; retrying with a fresh token")
			return 0
		}
	default:
		m.stopHeartbeat()
	}
	// Someone else holds it (-4 with a token we never had, -7, ...).
	if err := m.jr.clean(); err != nil {
		m.setState(StateNotConnected, "journal write failed: "+err.Error())
		return m.nextBackoff()
	}
	holder := m.observeHolder(ctx)
	m.setStatus(func(st *Status) {
		st.State, st.Holder = StateHeldByOther, holder
		st.Detail = fmt.Sprintf("RESERVE status %d", code)
	})
	return m.nextBackoff()
}

func (m *Manager) discoveryOpts(tok [8]byte, timeout time.Duration) vens.DiscoveryOptions {
	return vens.DiscoveryOptions{
		ScannerIP: m.cfg.ScannerIP, Token: tok, Timeout: timeout,
		ScannerPort: m.cfg.DiscoveryPort, ListenPort: m.cfg.DiscoveryReplyPort,
	}
}

// observeHolder asks with a throwaway token who holds the scanner.
func (m *Manager) observeHolder(ctx context.Context) string {
	info, err := vens.FindScanner(ctx, m.discoveryOpts(vens.NewToken(), 2*time.Second))
	if err != nil || info.State != 3 {
		return ""
	}
	return info.ClientIP
}

func (m *Manager) setup(ctx context.Context) {
	dc := vens.NewDataChannel(m.cfg.ScannerIP, uint16(m.dataPort), m.token).WithContext(ctx)
	dev, err := dc.GetDeviceInfo()
	if err != nil {
		m.log.Warn("device info", "err", err)
	} else {
		m.setStatus(func(st *Status) { st.DeviceName, st.Firmware = dev.DeviceName, dev.FirmwareRevision })
	}
	if !m.poll(ctx) || ctx.Err() != nil {
		return
	}
	if _, err := dc.GetScanParams(); err != nil {
		m.log.Warn("scan params", "err", err)
	}
	if _, err := dc.SetConfig(); err != nil {
		m.log.Warn("set config", "err", err)
	}
}

// poll refreshes paper/cover/Wi-Fi status. After three consecutive failures
// the session is considered lost (the journal stays dirty, so the next
// claim reuses the token). It reports whether the session is still held.
func (m *Manager) poll(ctx context.Context) bool {
	dc := vens.NewDataChannel(m.cfg.ScannerIP, uint16(m.dataPort), m.token).WithContext(ctx)
	adf, adfErr := dc.CheckADFStatus()
	wifi, wifiErr := m.control.WithContext(ctx).CheckStatus(m.token)
	if ctx.Err() != nil {
		return true // shutting down; not a liveness failure
	}
	if adfErr != nil && wifiErr != nil {
		m.pollFails++
		m.log.Warn("status poll failed", "fails", m.pollFails, "err", adfErr)
		if m.pollFails >= 3 {
			m.stopHeartbeat()
			m.claimed = false
			m.setState(StateConnecting, "lost contact with scanner")
			return false
		}
		return true
	}
	m.pollFails = 0
	m.setStatus(func(st *Status) {
		st.LastChecked = time.Now()
		if adfErr == nil {
			st.Paper, st.CoverOpen, st.Jam = adf.HasPaper, adf.HasCoverOpen, adf.HasJam
		}
		if wifiErr == nil {
			st.WifiSignal = int(wifi)
		}
	})
	return true
}

func (m *Manager) runScan(runCtx context.Context, job scanJob) {
	if !m.claimed {
		job.resp <- ScanResult{Err: ErrNotReady}
		return
	}
	m.setState(StateScanning, "")
	var res ScanResult
	// Back to claimed before answering, so the caller can scan again at once.
	defer func() {
		if m.claimed {
			m.setState(StateClaimed, "")
		}
		job.resp <- res
	}()

	ctx, cancel := context.WithCancel(job.ctx)
	defer cancel()
	// On shutdown, let a running batch finish for ScanGrace, then abort it.
	stopGrace := context.AfterFunc(runCtx, func() { time.AfterFunc(m.cfg.ScanGrace, cancel) })
	defer stopGrace()

	dc := vens.NewDataChannel(m.cfg.ScannerIP, uint16(m.dataPort), m.token)
	sess, err := dc.StartScanContext(ctx, job.req.Config)
	if err != nil {
		var se *vens.ScanError
		if errors.As(err, &se) && se.Kind == vens.ScanErrNoPaper {
			m.setStatus(func(st *Status) { st.Paper = false })
		}
		res.Err = err
		return
	}
	for {
		p, err := sess.NextPage()
		if len(p.JPEG) > 0 {
			if cbErr := job.req.OnPage(Page{Sheet: p.Sheet, Side: p.Side, Data: p.JPEG}); cbErr != nil {
				res.Err = cbErr
				break
			}
			res.Pages++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			res.Err = err
			break
		}
	}
	if err := sess.Close(); err != nil && ctx.Err() == nil {
		m.log.Debug("end scan", "err", err)
	}
	m.log.Info("scan finished", "pages", res.Pages, "err", res.Err)
}

func (m *Manager) startHeartbeat(ctx context.Context) error {
	m.stopHeartbeat()
	// The heartbeat must outlive Run's ctx until RELEASE has been sent.
	hb, err := vens.StartHeartbeatOpts(context.WithoutCancel(ctx), m.cfg.ScannerIP, m.token, vens.HeartbeatOptions{
		Interval: m.cfg.HeartbeatInterval, ScannerPort: m.cfg.DiscoveryPort, ReplyPort: m.cfg.DiscoveryReplyPort,
	})
	if err != nil {
		return err
	}
	m.hb = hb
	return nil
}

func (m *Manager) stopHeartbeat() {
	if m.hb != nil {
		m.hb.Stop()
		m.hb = nil
	}
}

// Release lets go of the scanner (normal RELEASE) so another computer can
// claim it, and keeps scan-hub off it, across restarts, until TakeBack. It
// returns ErrBusy during a scan.
func (m *Manager) Release(ctx context.Context) error { return m.request(ctx, true) }

// TakeBack ends a Release: scan-hub claims the scanner again as soon as it
// is free.
func (m *Manager) TakeBack(ctx context.Context) error { return m.request(ctx, false) }

func (m *Manager) request(ctx context.Context, release bool) error {
	if release && m.Status().State == StateScanning {
		return ErrBusy
	}
	req := ctlReq{release: release, resp: make(chan error, 1)}
	select {
	case m.ctl <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release runs on the actor.
func (m *Manager) release(ctx context.Context) error {
	if m.released {
		// Already off the scanner; make sure a restart remembers it (an
		// earlier write may have failed).
		return m.jr.setReleased(true)
	}
	if m.claimed {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		err := m.control.WithContext(c).Deregister(m.token)
		cancel()
		m.claimed = false
		if err != nil {
			// The session expires on its own once the heartbeat stops; the
			// dirty journal lets TakeBack reuse its token meanwhile.
			m.log.Warn("RELEASE failed; the session will expire on its own", "err", err)
		} else if err := m.jr.clean(); err != nil {
			m.log.Warn("journal clean", "err", err)
		}
	}
	m.stopHeartbeat()
	m.released = true
	m.setState(StateReleased, "")
	if err := m.jr.setReleased(true); err != nil {
		return fmt.Errorf("released, but not remembered across restarts: %w", err)
	}
	m.log.Info("released on request")
	return nil
}

// takeBack runs on the actor; changed reports that a claim should start.
func (m *Manager) takeBack() (changed bool, err error) {
	if !m.released {
		return false, nil
	}
	if err := m.jr.setReleased(false); err != nil {
		return false, err
	}
	m.released = false
	m.setState(StateConnecting, "")
	m.log.Info("taking the scanner back")
	return true, nil
}

// shutdown releases with its own bounded context (Run's ctx is already
// cancelled): RELEASE, then stop the heartbeat, then mark the journal clean.
func (m *Manager) shutdown() {
	m.setState(StateStopping, "")
	if m.claimed {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := m.control.WithContext(ctx).Deregister(m.token)
		cancel()
		m.stopHeartbeat()
		m.claimed = false
		if err != nil {
			// Journal stays dirty: the next start reuses this token.
			m.log.Warn("RELEASE failed; journal kept for recovery", "err", err)
		} else if err := m.jr.clean(); err != nil {
			m.log.Warn("journal clean", "err", err)
		} else {
			m.log.Info("released scanner")
		}
	}
	m.stopHeartbeat()
	m.setState(StateStopped, "")
}

// --- button events ---

// listenButtons subscribes to the hub (a private one without Config.Hub)
// and forwards debounced presses to m.buttons. The returned stop waits for
// the forwarder, so m.buttons can be closed after it.
func (m *Manager) listenButtons(ctx context.Context) (func(), error) {
	hub, own := m.cfg.Hub, false
	if hub == nil {
		h := NewNotifyHub(m.cfg.NotifyPort, m.cfg.Logger)
		if err := h.Listen(); err != nil {
			return nil, err
		}
		hub, own = h, true
	} else if m.cfg.NotifyPort != 0 && m.cfg.NotifyPort != hub.Port() {
		return nil, fmt.Errorf("scansnap: NotifyPort %d conflicts with the hub port %d", m.cfg.NotifyPort, hub.Port())
	}
	events, unsubscribe, err := hub.Subscribe(m.cfg.ScannerIP)
	if err != nil {
		if own {
			hub.Close()
		}
		return nil, err
	}
	m.notifyPort = hub.Port()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var lastSeq uint32
		var lastAt time.Time
		for ev := range events {
			if ev.Announce {
				select {
				case m.wake <- struct{}{}:
				default:
				}
				continue
			}
			seq := ev.Seq
			now := time.Now()
			if seq == lastSeq && now.Sub(lastAt) < m.cfg.ButtonDebounce {
				continue
			}
			lastSeq, lastAt = seq, now
			m.log.Info("scan button pressed", "seq", seq)
			select {
			case m.buttons <- ButtonEvent{At: now, Seq: seq}:
			case <-ctx.Done():
			default:
				m.log.Warn("button event dropped (consumer busy)")
			}
		}
	}()
	return func() {
		unsubscribe()
		if own {
			hub.Close()
		}
		<-done
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
