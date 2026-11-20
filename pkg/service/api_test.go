package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	s.opsFn = func(string) deviceOps { return &fakeOps{} }
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

func TestStoredAPKInstall(t *testing.T) {
	s, srv := testService(t)
	var mu sync.Mutex
	installs := 0
	s.installFn = func(ctx context.Context, device, path string) (string, error) {
		mu.Lock()
		installs++
		mu.Unlock()
		return "Success", nil
	}
	ctx := context.Background()
	alice := &client.Client{Base: srv.URL, Token: "a", HTTP: srv.Client()}
	l, err := alice.Acquire(ctx, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.InstallStored(ctx, l.ID, "android-f1"); !errors.Is(err, client.ErrAPKNotStored) {
		t.Fatalf("missing key: %v, want ErrAPKNotStored", err)
	}
	if err := alice.PutAPK(ctx, "android-f1", strings.NewReader("apk bytes")); err != nil {
		t.Fatal(err)
	}
	if err := alice.PutAPK(ctx, "android-f1", strings.NewReader("other")); !errors.Is(err, client.ErrAPKExists) {
		t.Fatalf("second put: %v, want ErrAPKExists", err)
	}
	list, err := alice.APKs(ctx)
	if err != nil || len(list) != 1 || list[0].Key != "android-f1" {
		t.Fatalf("list: %+v %v", list, err)
	}
	for i, want := range []bool{true, false} {
		got, err := alice.InstallStored(ctx, l.ID, "android-f1")
		if err != nil || got != want {
			t.Fatalf("install %d: installed=%v err=%v, want %v", i, got, err, want)
		}
	}
	if installs != 1 {
		t.Fatalf("adb install ran %d times, want 1", installs)
	}
	ds, _ := alice.Devices(ctx)
	if ds[0].APK != "android-f1" {
		t.Fatalf("device apk = %q", ds[0].APK)
	}
	// An ad hoc install replaces the app, so the device no longer matches the key.
	if _, err := alice.Install(ctx, l.ID, strings.NewReader("adhoc")); err != nil {
		t.Fatal(err)
	}
	if ds, _ := alice.Devices(ctx); ds[0].APK != "" {
		t.Fatalf("ad hoc install must clear device apk, got %q", ds[0].APK)
	}
	if got, _ := alice.InstallStored(ctx, l.ID, "android-f1"); !got {
		t.Fatal("stored apk must reinstall after an ad hoc install")
	}
	// A wipe removes installed apps.
	s.set(s.devs["p1"], func(st *DeviceStatus) { st.APK = "android-f1" })
	s.ctx = ctx
	s.reset(s.devs["p1"], true)
	s.halt(s.devs["p1"])
	if ds, _ := alice.Devices(ctx); ds[0].APK != "" {
		t.Fatalf("wipe must clear device apk, got %q", ds[0].APK)
	}
}

func scrape(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMetricsEndpoint(t *testing.T) {
	s, srv := testService(t)
	s.installFn = func(ctx context.Context, device, path string) (string, error) { return "Success", nil }
	ctx := context.Background()
	alice := &client.Client{Base: srv.URL, Token: "a", HTTP: srv.Client()}
	if code, _ := scrape(t, srv.URL, ""); code != http.StatusUnauthorized {
		t.Fatalf("metrics on the API listener need a token, got %d", code)
	}
	l, err := alice.Acquire(ctx, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	alice.PutAPK(ctx, "android-f1", strings.NewReader("apk"))
	alice.PutAPK(ctx, "android-f1", strings.NewReader("apk"))
	alice.InstallStored(ctx, l.ID, "android-f1")
	alice.InstallStored(ctx, l.ID, "android-f1")
	alice.Release(ctx, l.ID)
	time.Sleep(100 * time.Millisecond) // release cleanup runs async
	code, body := scrape(t, srv.URL, "a")
	if code != 200 {
		t.Fatalf("scrape: %d %s", code, body)
	}
	for _, want := range []string{
		`emuctl_build_info{version="dev"} 1`,
		`emuctl_device_state{device="p1",state="booted"} 1`,
		`emuctl_device_state{device="p1",state="crashed"} 0`,
		`emuctl_device_leased{device="p1"} 0`,
		`emuctl_leases_granted_total{device="p1"} 1`,
		`emuctl_leases_ended_total{device="p1",reason="released"} 1`,
		`emuctl_lease_queue_wait_seconds_count{device="p1"} 1`,
		`emuctl_health_checks_total{device="p1",trigger="release",result="healthy"} 1`,
		`emuctl_release_cleanup_seconds_count{device="p1"} 1`,
		`emuctl_apk_uploads_total{result="stored"} 1`,
		`emuctl_apk_uploads_total{result="exists"} 1`,
		`emuctl_apk_installs_total{device="p1",source="store",result="installed"} 1`,
		`emuctl_apk_installs_total{device="p1",source="store",result="skipped"} 1`,
		`emuctl_apk_store_entries 1`,
		`emuctl_queue_length{want="any"} 0`,
		`emuctl_http_requests_total{route="POST /v1/leases",code="200"} 1`,
		`emuctl_http_requests_total{route="PUT /v1/apks/{key}",code="409"} 1`,
		`emuctl_adb_forward_connections_total{device="p1",result="refused"} 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	// Lease IDs and APK keys must never appear as labels.
	if strings.Contains(body, l.ID) || strings.Contains(body, "android-f1") {
		t.Error("high-cardinality value leaked into metrics")
	}
}
