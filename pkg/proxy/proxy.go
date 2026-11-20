// Package proxy forwards TCP connections to a loopback emulator port.
//
// Emulators only bind loopback. Clients on other machines reach a device with
// `adb connect HOST:EXPOSE_PORT`, which this forwarder carries to the
// emulator's own adb port (console+1).
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

type Forwarder struct {
	Listen string // host:port to accept on
	Target string // host:port to dial
	Log    *slog.Logger
	// Allow decides per connection by client IP. nil admits nobody.
	Allow func(ip string) bool

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// CloseAll drops every open connection (lease ended or changed hands).
func (f *Forwarder) CloseAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		c.Close()
	}
}

func (f *Forwarder) track(c net.Conn, add bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns == nil {
		f.conns = map[net.Conn]struct{}{}
	}
	if add {
		f.conns[c] = struct{}{}
	} else {
		delete(f.conns, c)
	}
}

// Serve accepts until ctx is cancelled.
func (f *Forwarder) Serve(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", f.Listen)
	if err != nil {
		return err
	}
	return f.ServeListener(ctx, ln)
}

func (f *Forwarder) ServeListener(ctx context.Context, ln net.Listener) error {
	logger := f.Log
	if logger == nil {
		logger = slog.Default()
	}
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			logger.Warn("accept", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.handle(ctx, c, logger)
		}()
	}
}

func (f *Forwarder) handle(ctx context.Context, c net.Conn, logger *slog.Logger) {
	defer c.Close()
	ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	if f.Allow == nil || !f.Allow(ip) {
		logger.Info("refused: no active lease for client", "client", ip)
		return
	}
	f.track(c, true)
	defer f.track(c, false)
	d := net.Dialer{Timeout: 5 * time.Second}
	up, err := d.DialContext(ctx, "tcp", f.Target)
	if err != nil {
		logger.Warn("dial target", "target", f.Target, "client", c.RemoteAddr(), "err", err)
		return
	}
	defer up.Close()
	f.track(up, true)
	defer f.track(up, false)
	logger.Info("connected", "client", c.RemoteAddr(), "target", f.Target)
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if t, ok := dst.(*net.TCPConn); ok {
			t.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	select {
	case <-done:
		<-done
	case <-ctx.Done():
	}
}
