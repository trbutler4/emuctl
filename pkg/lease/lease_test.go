package lease

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(devs ...string) (*Manager, *clock) {
	m := NewManager(devs, time.Minute)
	c := &clock{t: time.Unix(1000, 0)}
	m.now = c.now
	return m, c
}

func TestFIFOAndRelease(t *testing.T) {
	m, _ := setup("a")
	l1, _ := m.Request("u1", "", "1.1.1.1", "")
	l2, _ := m.Request("u2", "", "2.2.2.2", "")
	l3, _ := m.Request("u3", "a", "3.3.3.3", "")
	if l1.State != Active || l1.Device != "a" {
		t.Fatalf("first should be granted: %+v", l1)
	}
	if l2.State != Queued || l2.Position != 1 || l3.Position != 2 {
		t.Fatalf("queue order wrong: %+v %+v", l2, l3)
	}
	m.Release(l1.ID)
	if g, _ := m.Get(l2.ID, 0); g.State != Active {
		t.Fatalf("l2 should be next: %+v", g)
	}
	if g, _ := m.Get(l3.ID, 0); g.Position != 1 {
		t.Fatalf("l3 should move up: %+v", g)
	}
}

func TestSpecificWaiterReservesDevice(t *testing.T) {
	m, _ := setup("a", "b")
	la, _ := m.Request("u1", "a", "", "")
	lb, _ := m.Request("u2", "b", "", "")
	wantA, _ := m.Request("u3", "a", "", "")
	anyL, _ := m.Request("u4", "", "", "")
	m.Release(lb.ID)
	// b frees: the "any" waiter takes it, the a-waiter keeps waiting for a.
	if g, _ := m.Get(anyL.ID, 0); g.State != Active || g.Device != "b" {
		t.Fatalf("any waiter should get b: %+v", g)
	}
	m.Release(la.ID)
	if g, _ := m.Get(wantA.ID, 0); g.State != Active || g.Device != "a" {
		t.Fatalf("a waiter should get a: %+v", g)
	}
}

func TestAnyDoesNotJumpSpecific(t *testing.T) {
	m, _ := setup("a")
	l1, _ := m.Request("u1", "", "", "")
	want, _ := m.Request("u2", "a", "", "")
	anyL, _ := m.Request("u3", "", "", "")
	m.Release(l1.ID)
	if g, _ := m.Get(want.ID, 0); g.State != Active {
		t.Fatal("earlier specific waiter must win")
	}
	if g, _ := m.Get(anyL.ID, 0); g.State != Queued {
		t.Fatal("later any waiter must still wait")
	}
}

func TestExpiryAndRenew(t *testing.T) {
	m, c := setup("a")
	l1, _ := m.Request("u1", "", "", "")
	l2, _ := m.Request("u2", "", "", "")
	c.t = c.t.Add(50 * time.Second)
	m.Renew(l1.ID)
	m.Renew(l2.ID)
	c.t = c.t.Add(50 * time.Second)
	m.Sweep()
	if g, _ := m.Get(l1.ID, 0); g.State != Active {
		t.Fatal("renewed lease must survive")
	}
	c.t = c.t.Add(2 * time.Minute)
	m.Renew(l2.ID) // waiter still polling
	m.Sweep()
	if g, _ := m.Get(l1.ID, 0); g.State != Expired {
		t.Fatalf("stale lease must expire: %+v", g)
	}
	if g, _ := m.Get(l2.ID, 0); g.State != Active {
		t.Fatalf("next waiter granted after expiry: %+v", g)
	}
}

func TestQueuedWaiterAbandons(t *testing.T) {
	m, c := setup("a")
	m.Request("u1", "", "", "")
	l2, _ := m.Request("u2", "", "", "")
	c.t = c.t.Add(2 * time.Minute)
	m.Sweep()
	if g, _ := m.Get(l2.ID, 0); g.State != Expired {
		t.Fatalf("silent waiter must be dropped: %+v", g)
	}
}

func TestBusyBlocksGrant(t *testing.T) {
	m, _ := setup("a")
	m.SetBusy("a", true)
	l, _ := m.Request("u1", "", "", "")
	if l.State != Queued {
		t.Fatal("busy device must not be granted")
	}
	m.SetBusy("a", false)
	if g, _ := m.Get(l.ID, 0); g.State != Active {
		t.Fatal("grant after device frees")
	}
}

func TestLongPollWakes(t *testing.T) {
	m, _ := setup("a")
	l1, _ := m.Request("u1", "", "", "")
	l2, _ := m.Request("u2", "", "", "")
	go func() { time.Sleep(50 * time.Millisecond); m.Release(l1.ID) }()
	start := time.Now()
	g, _ := m.Get(l2.ID, 5*time.Second)
	if g.State != Active || time.Since(start) > 2*time.Second {
		t.Fatalf("long poll should wake on grant: %+v", g)
	}
}

func TestUnknownDevice(t *testing.T) {
	m, _ := setup("a")
	if _, err := m.Request("u", "zzz", "", ""); err == nil {
		t.Fatal("want error")
	}
}
