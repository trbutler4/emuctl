// Package lease is a FIFO queue of exclusive device leases.
//
// A request names a device or "" for any free device. Waiters are served in
// arrival order per device; an "any" waiter takes the first device that frees
// up unless an earlier waiter targets that device. Leases expire unless
// renewed, so a crashed agent never holds a device forever.
package lease

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

type State string

const (
	Queued   State = "queued"
	Active   State = "active"
	Released State = "released"
	Expired  State = "expired"
)

type Lease struct {
	ID       string    `json:"id"`
	User     string    `json:"user"`
	Want     string    `json:"want"` // requested device, "" = any
	Device   string    `json:"device,omitempty"`
	State    State     `json:"state"`
	ClientIP string    `json:"client_ip"`
	Note     string    `json:"note,omitempty"`
	Queued   time.Time `json:"queued_at"`
	Granted  time.Time `json:"granted_at,omitzero"`
	Expires  time.Time `json:"expires_at,omitzero"`
	Position int       `json:"position,omitempty"` // 1-based place in queue while queued
	TTL      Duration  `json:"ttl"`

	seen time.Time // last request/renew from the client

	changed chan struct{} // closed on every state change
}

var ErrNotFound = errors.New("lease not found")

// Hooks lets the owner react to grants and ends (open/close adb, reset).
type Hooks struct {
	Granted func(l Lease)
	Ended   func(l Lease)
}

type Manager struct {
	mu      sync.Mutex
	devices []string
	busy    map[string]bool // device unavailable (e.g. resetting, not booted)
	maint   map[string]bool // held for maintenance (release cleanup, health check)
	holder  map[string]*Lease
	queue   []*Lease
	all     map[string]*Lease
	TTL     time.Duration
	Hooks   Hooks
	// HoldOnEnd keeps a device out of the pool when its lease ends until the
	// owner calls Done, so the Ended hook finishes cleanup before anyone else
	// is granted the device.
	HoldOnEnd bool
	now       func() time.Time
}

func NewManager(devices []string, ttl time.Duration) *Manager {
	return &Manager{devices: devices, busy: map[string]bool{}, maint: map[string]bool{}, holder: map[string]*Lease{},
		all: map[string]*Lease{}, TTL: ttl, now: time.Now}
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) known(dev string) bool {
	for _, d := range m.devices {
		if d == dev {
			return true
		}
	}
	return false
}

// Duration marshals as seconds.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%d", int64(time.Duration(d)/time.Second))), nil
}

// Request enqueues a lease and grants it immediately when possible. ttl 0
// uses the manager default.
func (m *Manager) Request(user, want, ip, note string) (Lease, error) {
	return m.RequestTTL(user, want, ip, note, 0)
}

func (m *Manager) RequestTTL(user, want, ip, note string, ttl time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if want != "" && !m.known(want) {
		return Lease{}, fmt.Errorf("unknown device %q", want)
	}
	if ttl <= 0 {
		ttl = m.TTL
	}
	l := &Lease{ID: newID(), User: user, Want: want, ClientIP: ip, Note: note, TTL: Duration(ttl),
		State: Queued, Queued: m.now(), seen: m.now(), changed: make(chan struct{})}
	m.all[l.ID] = l
	m.queue = append(m.queue, l)
	m.dispatch()
	return m.view(l), nil
}

// dispatch grants queued leases to free devices in FIFO order. Caller holds mu.
func (m *Manager) dispatch() {
	free := map[string]bool{}
	for _, d := range m.devices {
		free[d] = m.holder[d] == nil && !m.busy[d] && !m.maint[d]
	}
	rest := m.queue[:0]
	for _, l := range m.queue {
		dev := ""
		if l.Want != "" {
			if free[l.Want] {
				dev = l.Want
			}
		} else {
			for _, d := range m.devices {
				if free[d] {
					dev = d
					break
				}
			}
		}
		if dev == "" {
			// A specific waiter reserves its device against later "any" waiters.
			if l.Want != "" {
				free[l.Want] = false
			}
			rest = append(rest, l)
			continue
		}
		free[dev] = false
		l.Device, l.State, l.Granted, l.Expires = dev, Active, m.now(), m.now().Add(time.Duration(l.TTL))
		m.holder[dev] = l
		m.notify(l)
		if m.Hooks.Granted != nil {
			go m.Hooks.Granted(*l)
		}
	}
	m.queue = rest
	for _, l := range m.queue {
		m.notify(l) // positions moved
	}
}

func (m *Manager) notify(l *Lease) {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (m *Manager) view(l *Lease) Lease {
	v := *l
	v.changed = nil
	if l.State == Queued {
		for i, q := range m.queue {
			if q == l {
				v.Position = i + 1
			}
		}
	}
	return v
}

// Get returns a lease, optionally blocking until it changes or wait elapses.
func (m *Manager) Get(id string, wait time.Duration) (Lease, error) {
	m.mu.Lock()
	l, ok := m.all[id]
	if !ok {
		m.mu.Unlock()
		return Lease{}, ErrNotFound
	}
	if wait <= 0 || l.State != Queued {
		defer m.mu.Unlock()
		return m.view(l), nil
	}
	l.seen = m.now()
	ch := l.changed
	m.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(wait):
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.view(l), nil
}

// Renew extends an active lease (heartbeat). Queued leases are kept alive too.
func (m *Manager) Renew(id string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.all[id]
	if !ok || (l.State != Active && l.State != Queued) {
		return Lease{}, ErrNotFound
	}
	l.seen = m.now()
	if l.State == Active {
		l.Expires = m.now().Add(time.Duration(l.TTL))
	}
	return m.view(l), nil
}

func (m *Manager) Release(id string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.all[id]
	if !ok {
		return Lease{}, ErrNotFound
	}
	m.end(l, Released)
	return m.view(l), nil
}

func (m *Manager) end(l *Lease, st State) {
	switch l.State {
	case Queued:
		for i, q := range m.queue {
			if q == l {
				m.queue = append(m.queue[:i], m.queue[i+1:]...)
				break
			}
		}
	case Active:
		delete(m.holder, l.Device)
		if m.HoldOnEnd {
			m.maint[l.Device] = true
		}
		if m.Hooks.Ended != nil {
			v := *l
			v.State = st
			go m.Hooks.Ended(v)
		}
	default:
		return
	}
	l.State = st
	m.notify(l)
	m.dispatch()
}

// Sweep expires stale leases: active ones past Expires, queued ones whose
// waiter stopped polling for TTL. Call periodically.
func (m *Manager) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	// Drop silent waiters first so an expiring holder never hands its device
	// to an abandoned waiter.
	for _, l := range append([]*Lease(nil), m.queue...) {
		if now.Sub(l.seen) > time.Duration(l.TTL) {
			m.end(l, Expired)
		}
	}
	for _, l := range m.all {
		switch {
		case l.State == Active && now.After(l.Expires):
			m.end(l, Expired)
		case (l.State == Released || l.State == Expired) && now.Sub(l.Queued) > 24*time.Hour:
			delete(m.all, l.ID)
		}
	}
}

// Hold takes a free device out of the pool for maintenance, such as an idle
// health check. It reports false if the device is leased, busy or already
// held. Call Done when finished.
func (m *Manager) Hold(dev string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holder[dev] != nil || m.busy[dev] || m.maint[dev] {
		return false
	}
	m.maint[dev] = true
	return true
}

// Done returns a device held by Hold or HoldOnEnd to the pool.
func (m *Manager) Done(dev string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maint[dev] {
		delete(m.maint, dev)
		m.dispatch()
	}
}

// SetBusy marks a device unavailable for new grants (booting, resetting).
func (m *Manager) SetBusy(dev string, busy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy[dev] = busy
	if !busy {
		m.dispatch()
	}
}

// Holder returns the active lease on a device.
func (m *Manager) Holder(dev string) (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.holder[dev]
	if l == nil {
		return Lease{}, false
	}
	return m.view(l), true
}

// Queue lists queued leases in order.
func (m *Manager) Queue() []Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Lease{}
	for _, l := range m.queue {
		out = append(out, m.view(l))
	}
	return out
}
