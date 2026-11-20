package proxy

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestAllowGateAndCloseAll(t *testing.T) {
	var allowed atomic.Bool
	f := &Forwarder{Target: echoServer(t), Allow: func(string) bool { return allowed.Load() }}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.ServeListener(ctx, ln)

	try := func() (net.Conn, bool) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		c.Write([]byte("ping"))
		buf := make([]byte, 4)
		_, err = io.ReadFull(c, buf)
		return c, err == nil && string(buf) == "ping"
	}
	if c, ok := try(); ok {
		t.Fatal("refused client got through")
	} else {
		c.Close()
	}
	allowed.Store(true)
	c, ok := try()
	if !ok {
		t.Fatal("allowed client blocked")
	}
	f.CloseAll()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("CloseAll must drop the connection")
	}
}
