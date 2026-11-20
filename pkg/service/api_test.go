package service

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trbutler4/emuctl/pkg/client"
	"github.com/trbutler4/emuctl/pkg/config"
)

func testService(t *testing.T) (*Service, *httptest.Server) {
	cfg, err := config.Parse([]byte(`{"sdk":"/nonexistent","state_dir":"` + t.TempDir() + `",
	  "users":{"alice":"` + config.HashToken("a") + `","bob":"` + config.HashToken("b") + `"},
	  "devices":{"p1":{"expose_port":15601}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Pretend p1 booted, without launching anything.
	s.devs["p1"].st.State = "booted"
	s.Leases.SetBusy("p1", false)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func TestQueueAcrossUsers(t *testing.T) {
	_, srv := testService(t)
	ctx := context.Background()
	alice := &client.Client{Base: srv.URL, Token: "a", HTTP: srv.Client()}
	bob := &client.Client{Base: srv.URL, Token: "b", HTTP: srv.Client()}
	anon := &client.Client{Base: srv.URL, Token: "x", HTTP: srv.Client()}

	if _, err := anon.Devices(ctx); err == nil {
		t.Fatal("bad token must be rejected")
	}
	la, err := alice.Acquire(ctx, "", "alice test", nil)
	if err != nil || la.Device != "p1" || !la.Ready {
		t.Fatalf("alice acquire: %+v %v", la, err)
	}
	if la.ADB == "" {
		t.Fatal("lease must carry adb target")
	}

	got := make(chan client.Lease, 1)
	go func() {
		l, err := bob.Acquire(ctx, "p1", "bob test", nil)
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	time.Sleep(200 * time.Millisecond)
	q, _ := alice.Queue(ctx)
	if len(q) != 1 || q[0].User != "bob" {
		t.Fatalf("bob should be queued: %+v", q)
	}
	if err := bob.Release(ctx, la.ID); err == nil {
		t.Fatal("bob must not release alice's lease")
	}
	if _, err := bob.Reset(ctx, la.ID, true); err == nil {
		t.Fatal("bob must not reset alice's device")
	}
	if err := alice.Release(ctx, la.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case lb := <-got:
		if lb.State != "active" || lb.User != "bob" {
			t.Fatalf("bob lease: %+v", lb)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bob never got the device")
	}
	ds, _ := alice.Devices(ctx)
	if ds[0].Holder == nil || ds[0].Holder.User != "bob" {
		t.Fatalf("holder should be bob: %+v", ds[0])
	}
}

func TestOpenModeNeedsNoToken(t *testing.T) {
	cfg, err := config.Parse([]byte(`{"sdk":"/nonexistent","state_dir":"` + t.TempDir() + `","devices":{"p1":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	c := &client.Client{Base: srv.URL, HTTP: srv.Client()}
	ds, err := c.Devices(context.Background())
	if err != nil || len(ds) != 1 {
		t.Fatalf("devices without token: %v %v", ds, err)
	}
}
