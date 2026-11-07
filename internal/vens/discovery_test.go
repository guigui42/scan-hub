package vens_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/guigui42/scan-hub/internal/vens"
	"github.com/guigui42/scan-hub/internal/vens/venstest"
)

func freePort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// Concurrent discoveries of two scanners take turns on one reply port, and
// each gets its own scanner's reply.
func TestDiscoverySharesReplyPort(t *testing.T) {
	var fakes []*venstest.Scanner
	for _, ip := range []string{"127.0.0.2", "127.0.0.3"} {
		f, err := venstest.Start(venstest.Options{Password: "1", IP: ip})
		if err != nil {
			t.Skipf("cannot bind %s (needs Linux or a lo0 alias): %v", ip, err)
		}
		t.Cleanup(f.Close)
		fakes = append(fakes, f)
	}
	port := freePort(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 10 {
		f := fakes[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := vens.FindScanner(context.Background(), vens.DiscoveryOptions{
				ScannerIP: f.IP(), Token: vens.NewToken(), Timeout: 2 * time.Second,
				ScannerPort: f.DiscoveryPort(), ListenPort: port,
			})
			if err != nil {
				errs <- err
				return
			}
			if info.DeviceIP != f.IP() {
				errs <- errors.New("reply from " + info.DeviceIP + " for " + f.IP())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A discovery waiting for the port returns as soon as its context ends.
func TestDiscoveryWaitHonoursContext(t *testing.T) {
	port := freePort(t)
	// Hold the port with a discovery towards a silent address.
	silent := net.IPv4(127, 0, 0, 1).String()
	holdCtx, stopHold := context.WithCancel(context.Background())
	held := make(chan struct{})
	go func() {
		close(held)
		vens.FindScanner(holdCtx, vens.DiscoveryOptions{ScannerIP: silent, Timeout: 5 * time.Second, ScannerPort: freePort(t), ListenPort: port})
	}()
	<-held
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := vens.FindScanner(ctx, vens.DiscoveryOptions{ScannerIP: silent, Timeout: 5 * time.Second, ScannerPort: freePort(t), ListenPort: port})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	stopHold()
}
