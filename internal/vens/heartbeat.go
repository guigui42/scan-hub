// Derived from github.com/mzyy94/AirScap internal/vens @ 177fdae.
// Copyright (c) 2026 mzyy94. MIT License; see THIRD_PARTY_NOTICES.

package vens

import (
	"context"
	"log/slog"
	"net"
	"time"
)

// Heartbeat sends periodic UDP heartbeat packets to keep the session alive.
type Heartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// HeartbeatOptions configures StartHeartbeat.
type HeartbeatOptions struct {
	Interval    time.Duration // 0 = 500 ms
	ScannerPort int           // 0 = DiscoveryPort (52217)
	ReplyPort   int           // port advertised in the packet; 0 = ClientDiscoveryPort (55264)
}

// StartHeartbeat begins sending heartbeat packets every interval to the scanner.
// Cancel the returned Heartbeat to stop.
func StartHeartbeat(ctx context.Context, scannerIP string, token [8]byte, interval time.Duration) (*Heartbeat, error) {
	return StartHeartbeatOpts(ctx, scannerIP, token, HeartbeatOptions{Interval: interval})
}

// StartHeartbeatOpts is StartHeartbeat with configurable ports.
func StartHeartbeatOpts(ctx context.Context, scannerIP string, token [8]byte, opts HeartbeatOptions) (*Heartbeat, error) {
	interval := opts.Interval
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	if opts.ScannerPort == 0 {
		opts.ScannerPort = DiscoveryPort
	}
	if opts.ReplyPort == 0 {
		opts.ReplyPort = ClientDiscoveryPort
	}

	localIP := GetLocalIP(scannerIP)
	packet := MarshalDiscoveryVENS(localIP, token, uint16(opts.ReplyPort), true)
	addr := &net.UDPAddr{IP: net.ParseIP(scannerIP), Port: opts.ScannerPort}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer conn.Close()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		slog.Debug("heartbeat started", "scanner", scannerIP, "interval", interval)
		for {
			conn.WriteToUDP(packet, addr)
			select {
			case <-ctx.Done():
				slog.Debug("heartbeat stopped")
				return
			case <-ticker.C:
			}
		}
	}()

	return &Heartbeat{cancel: cancel, done: done}, nil
}

// Stop stops the heartbeat.
func (h *Heartbeat) Stop() {
	h.cancel()
	<-h.done
}
