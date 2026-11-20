package service

import (
	"net/http"
	"strconv"
	"time"

	"github.com/trbutler4/emuctl/pkg/apkstore"
	"github.com/trbutler4/emuctl/pkg/metrics"
)

// deviceStates are every DeviceStatus.State value, so each device exports a
// 0/1 series per state and dashboards can sum by state without gaps.
var deviceStates = []string{"stopped", "starting", "booted", "crashed", "resetting"}

// Metrics are emuctl's Prometheus series. Labels are bounded: device names
// come from the config, and results/reasons are fixed strings. Lease IDs,
// users' tokens and APK keys are never labels.
type Metrics struct {
	reg *metrics.Registry

	leasesGranted *metrics.Counter   // device
	leasesEnded   *metrics.Counter   // device, reason (released|expired)
	queueWait     *metrics.Histogram // device: queued -> granted
	leaseDuration *metrics.Histogram // device, reason: granted -> ended

	boots        *metrics.Counter   // device, result (ok|timeout)
	bootDuration *metrics.Histogram // device
	exits        *metrics.Counter   // device: unexpected emulator exits
	restarts     *metrics.Counter   // device, reason (crash|broken|unverified|cleanup|wipe|request)
	reaped       *metrics.Counter   // device: leftover emulator processes stopped

	healthChecks *metrics.Counter // device, trigger (idle|release), result (healthy|unknown|broken)
	release      *metrics.Histogram
	releaseFails *metrics.Counter // device

	installs        *metrics.Counter   // device, source (store|upload), result (installed|skipped|failed)
	installDuration *metrics.Histogram // device, source
	apkUploads      *metrics.Counter   // result (stored|exists|rejected)

	http         *metrics.Counter   // route, code
	httpDuration *metrics.Histogram // route
}

func newMetrics(s *Service) *Metrics {
	r := metrics.NewRegistry()
	m := &Metrics{reg: r}
	m.leasesGranted = r.Counter("emuctl_leases_granted_total", "Leases granted.", "device")
	m.leasesEnded = r.Counter("emuctl_leases_ended_total", "Active leases ended, by reason.", "device", "reason")
	m.queueWait = r.Histogram("emuctl_lease_queue_wait_seconds", "Time from lease request to grant.", metrics.SlowBuckets, "device")
	m.leaseDuration = r.Histogram("emuctl_lease_duration_seconds", "Time a lease held its device.", metrics.SlowBuckets, "device", "reason")
	m.boots = r.Counter("emuctl_emulator_boots_total", "Emulator boots, by result.", "device", "result")
	m.bootDuration = r.Histogram("emuctl_emulator_boot_seconds", "Time from emulator start to booted and unlocked.", metrics.SlowBuckets, "device")
	m.exits = r.Counter("emuctl_emulator_exits_total", "Unexpected emulator exits.", "device")
	m.restarts = r.Counter("emuctl_device_restarts_total", "Device restarts initiated by emuctl, by reason.", "device", "reason")
	m.reaped = r.Counter("emuctl_stale_emulators_reaped_total", "Leftover emulator processes from an earlier emuctl that were stopped.", "device")
	m.healthChecks = r.Counter("emuctl_health_checks_total", "Device health checks, by trigger and result.", "device", "trigger", "result")
	m.release = r.Histogram("emuctl_release_cleanup_seconds", "Time from lease end until the device is back in the pool or restarting.", metrics.FastBuckets, "device")
	m.releaseFails = r.Counter("emuctl_release_cleanup_failures_total", "Release cleanups that failed and restarted the device.", "device")
	m.installs = r.Counter("emuctl_apk_installs_total", "APK installs, by source and result.", "device", "source", "result")
	m.installDuration = r.Histogram("emuctl_apk_install_seconds", "adb install duration.", metrics.SlowBuckets, "device", "source")
	m.apkUploads = r.Counter("emuctl_apk_uploads_total", "APK store uploads, by result.", "result")
	m.http = r.Counter("emuctl_http_requests_total", "API requests, by route pattern and status code.", "route", "code")
	m.httpDuration = r.Histogram("emuctl_http_request_seconds", "API request duration, by route pattern. Long polls included.", metrics.FastBuckets, "route")

	r.GaugeFunc("emuctl_build_info", "emuctl build. Always 1.", []string{"version"}, func(set func(float64, ...string)) {
		set(1, s.Version)
	})
	r.GaugeFunc("emuctl_device_state", "1 for the device's current state, 0 for the others.", []string{"device", "state"}, func(set func(float64, ...string)) {
		for _, name := range s.Cfg.Names() {
			d := s.devs[name]
			d.mu.Lock()
			cur := d.st.State
			d.mu.Unlock()
			for _, st := range deviceStates {
				v := 0.0
				if st == cur {
					v = 1
				}
				set(v, name, st)
			}
		}
	})
	r.GaugeFunc("emuctl_device_leased", "1 if the device has an active lease.", []string{"device"}, func(set func(float64, ...string)) {
		for _, name := range s.Cfg.Names() {
			_, held := s.Leases.Holder(name)
			set(b2f(held), name)
		}
	})
	r.GaugeFunc("emuctl_device_state_seconds", "Seconds since the device entered its current state.", []string{"device"}, func(set func(float64, ...string)) {
		for _, name := range s.Cfg.Names() {
			d := s.devs[name]
			d.mu.Lock()
			since := d.st.Since
			d.mu.Unlock()
			set(time.Since(since).Seconds(), name)
		}
	})
	r.GaugeFunc("emuctl_device_health_unverified", "Consecutive inconclusive health checks. Restarts at 3.", []string{"device"}, func(set func(float64, ...string)) {
		for _, name := range s.Cfg.Names() {
			d := s.devs[name]
			d.mu.Lock()
			n := d.st.Unverified
			d.mu.Unlock()
			set(float64(n), name)
		}
	})
	r.GaugeFunc("emuctl_device_last_healthy_timestamp_seconds", "Unix time of the last passing health check or boot, 0 if never.", []string{"device"}, func(set func(float64, ...string)) {
		for _, name := range s.Cfg.Names() {
			d := s.devs[name]
			d.mu.Lock()
			t := d.st.Checked
			d.mu.Unlock()
			set(unix(t), name)
		}
	})
	r.GaugeFunc("emuctl_queue_length", "Queued leases, by requested device (\"any\" for no preference).", []string{"want"}, func(set func(float64, ...string)) {
		n := map[string]int{"any": 0}
		for _, name := range s.Cfg.Names() {
			n[name] = 0
		}
		for _, l := range s.Leases.Queue() {
			w := l.Want
			if w == "" {
				w = "any"
			}
			n[w]++
		}
		for w, c := range n {
			set(float64(c), w)
		}
	})
	r.GaugeFunc("emuctl_queue_oldest_wait_seconds", "Age of the oldest queued lease, 0 when the queue is empty.", nil, func(set func(float64, ...string)) {
		oldest := 0.0
		for _, l := range s.Leases.Queue() {
			oldest = max(oldest, time.Since(l.Queued).Seconds())
		}
		set(oldest)
	})
	r.GaugeFunc("emuctl_apk_store_entries", "APKs in the store.", nil, func(set func(float64, ...string)) {
		n, _ := storeTotals(s.APKs)
		set(float64(n))
	})
	r.GaugeFunc("emuctl_apk_store_bytes", "Total size of APKs in the store.", nil, func(set func(float64, ...string)) {
		_, b := storeTotals(s.APKs)
		set(float64(b))
	})
	r.CounterFunc("emuctl_adb_forward_connections_total", "Connections to a device's adb forwarder, by result (accepted|refused).", []string{"device", "result"}, func(set func(float64, ...string)) {
		for _, name := range s.Cfg.Names() {
			if f := s.devs[name].fwd; f != nil {
				a, rf, _ := f.Stats()
				set(float64(a), name, "accepted")
				set(float64(rf), name, "refused")
			}
		}
	})
	r.GaugeFunc("emuctl_adb_forward_open_connections", "Connections being proxied to a device now.", []string{"device"}, func(set func(float64, ...string)) {
		for _, name := range s.Cfg.Names() {
			if f := s.devs[name].fwd; f != nil {
				_, _, o := f.Stats()
				set(float64(o), name)
			}
		}
	})
	return m
}

func storeTotals(st *apkstore.Store) (int, int64) {
	list, err := st.List()
	if err != nil {
		return 0, 0
	}
	var b int64
	for _, e := range list {
		b += e.Bytes
	}
	return len(list), b
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unix(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// statusRecorder captures the response code for request metrics.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// instrument records request count and latency by the mux route pattern, so
// path parameters (lease IDs, device names) never become labels.
func (m *Metrics) instrument(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		mux.ServeHTTP(rec, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		m.http.Inc(route, strconv.Itoa(rec.code))
		m.httpDuration.Observe(time.Since(start).Seconds(), route)
	})
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler { return m.reg.Handler() }
