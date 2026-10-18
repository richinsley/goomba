package goomba

import (
	"context"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richinsley/goomba/lock"
	"github.com/richinsley/goomba/virtual"
)

// countingConn counts the bytes read from a connection.
type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// TestLiveLockSharded locks python=3.12 against conda-forge's sharded index
// from a cold cache, then again from the warm cache, and reports the time,
// the bytes received (TLS included) and the peak Go heap.
func TestLiveLockSharded(t *testing.T) {
	if os.Getenv("GOOMBA_LIVE") == "" {
		t.Skip("set GOOMBA_LIVE=1")
	}
	plat := virtual.HostPlatform()
	if plat == "" {
		t.Skip("no host platform")
	}
	var received atomic.Int64
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	client := &http.Client{Transport: &http.Transport{
		// The network guard checks the address of every connection this
		// dialer makes.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return countingConn{c, &received}, nil
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 16,
		TLSHandshakeTimeout: 30 * time.Second,
	}}
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	opt := LockOptions{
		Specs:       []string{"python=3.12"},
		Channels:    []string{"conda-forge"},
		Virtual:     virtual.Defaults(plat),
		RepodataDir: t.TempDir(),
		Client:      client,
	}
	var events []Event
	opt.Progress = func(e Event) { events = append(events, e) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	lf, err := Lock(ctx, opt)
	cold := time.Since(start)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cold cache: %d packages in %v, %.1f MB received, peak heap %.0f MB; events %+v",
		len(lf.Packages), cold, float64(received.Load())/1e6, float64(peak.Load())/1e6, events)
	if lf.Channels[0] != "https://conda.anaconda.org/conda-forge" {
		t.Errorf("channels %q", lf.Channels)
	}
	var python *lock.Package
	for i := range lf.Packages {
		if lf.Packages[i].Name == "python" {
			python = &lf.Packages[i]
		}
	}
	if python == nil || python.Version[:5] != "3.12." {
		t.Errorf("python %+v", python)
	}
	before := received.Load()
	start = time.Now()
	opt.Progress = nil
	if _, err := Lock(ctx, opt); err != nil {
		t.Fatal(err)
	}
	t.Logf("warm cache: %v, %.1f MB received", time.Since(start), float64(received.Load()-before)/1e6)
}
