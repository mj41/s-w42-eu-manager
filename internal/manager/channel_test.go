package manager

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mj41/s-w42-eu-manager/internal/robotsim"
)

// serve runs a manager on a real HTTP server (robots open WebSockets) on loopback: its page's
// requests come from "this computer".
func serve(t *testing.T, cfg Config) (*Manager, string) {
	t.Helper()
	if cfg.AppsFile == "" {
		cfg.AppsFile = catalog(t)
	}
	if cfg.SigningKeyFile == "" {
		cfg.SigningKeyFile = filepath.Join(t.TempDir(), "key.pem")
	}
	m := newManager(t, cfg)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	m.cfg.PublicURL = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.RunLink(ctx)
	return m, srv.URL
}

// web is the page's request (same origin) to a manager at base.
func web(t *testing.T, base, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	req.Header.Set("Origin", base)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// eventually waits up to 5 s for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

// pageRobot is the robot on the page (GET /api/me).
func pageRobot(t *testing.T, base, id string) robotView {
	t.Helper()
	_, body := web(t, base, "GET", "/api/me", "")
	var me struct{ Robots []robotView }
	json.Unmarshal([]byte(body), &me)
	for _, r := range me.Robots {
		if r.ID == id {
			return r
		}
	}
	return robotView{}
}

// setUpSim sets a robot up over "USB" (the setup API) and starts it.
func setUpSim(t *testing.T, base, id, body string) *robotsim.Robot {
	t.Helper()
	code, res := web(t, base, "POST", "/api/my/robots/"+id+"/setup", body)
	if code != http.StatusOK {
		t.Fatalf("setup: %d %s", code, res)
	}
	r, err := robotsim.FromSetup(id, []byte(res))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Run(ctx)
	eventually(t, "the robot's channel", func() bool { return pageRobot(t, base, id).Online })
	return r
}

// S1, S5, S6, S7, S9: the robot's channel: live state, app lists, switches with the question and
// the answer, a switch on its screen, a hung robot restarted.
func TestChannel(t *testing.T) {
	_, base := serve(t, Config{})
	const id = "stackchan-000000000001"
	r := setUpSim(t, base, id, `{"apps":["raw","pet"],"start":"raw"}`)

	v := pageRobot(t, base, id)
	if v.LastApp != "raw" || v.AppName != "Raw data" || v.Applied != v.Version || v.Firmware != "1.5.1-mj41-sim" {
		t.Fatalf("live state: %+v", v)
	}

	// S5: apps change online; the robot gets a signed list at once, with the new app's token.
	if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/apps", `{"apps":["raw","pet","home"],"start":"pet"}`); code != http.StatusOK {
		t.Fatalf("apps: %d %s", code, body)
	}
	eventually(t, "the new list on the robot", func() bool { return len(r.Snapshot().Apps) == 3 })
	s := r.Snapshot()
	if s.Pin != "wss://pet.example" || s.Apps[2].Token != "shared-token" || len(s.Apps[1].Token) != 64 {
		t.Fatalf("robot: %+v", s)
	}
	eventually(t, "on the robot ✓", func() bool { v := pageRobot(t, base, id); return v.Applied == v.Version && len(v.Waiting) == 0 })

	// S6: a switch; the robot asks; nobody answers: "not confirmed".
	if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/switch", `{"app":"pet"}`); code != http.StatusOK || !strings.Contains(body, `"ask":true`) {
		t.Fatalf("switch: %d %s", code, body)
	}
	eventually(t, "not confirmed", func() bool { return pageRobot(t, base, id).Answer == "not confirmed" })
	// The person taps Yes.
	asked, release := make(chan string, 1), make(chan struct{})
	r.Answer = func(q string) bool { asked <- q; <-release; return true }
	web(t, base, "POST", "/api/my/robots/"+id+"/switch", `{"app":"pet"}`)
	eventually(t, "the question on the page", func() bool { q := pageRobot(t, base, id).Question; return q != nil && q.Text == "Connect to Pet?" })
	if q := <-asked; q != "Connect to Pet?" {
		t.Errorf("asked %q", q)
	}
	close(release) // the person taps Yes
	eventually(t, "switched", func() bool { v := pageRobot(t, base, id); return v.LastApp == "pet" && v.Answer == "switched" })

	// S7: on its screen; the page follows.
	r.SwitchOnScreen("wss://raw.example")
	eventually(t, "the page follows", func() bool { return pageRobot(t, base, id).LastApp == "raw" })

	// S9: stuck; restarted; back on its start app.
	r.SetStuck(true)
	eventually(t, "stuck on the page", func() bool { return pageRobot(t, base, id).Stuck })
	if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/restart", ""); code != http.StatusOK {
		t.Fatalf("restart: %d %s", code, body)
	}
	eventually(t, "restarted, back", func() bool {
		v := pageRobot(t, base, id)
		return r.Snapshot().Restarts == 1 && v.Online && !v.Stuck && v.LastApp == "pet"
	})
	if s := r.Snapshot(); len(s.Refused) != 0 {
		t.Errorf("refused: %v", s.Refused)
	}
	v = pageRobot(t, base, id)
	if !slices.ContainsFunc(v.History, func(e Event) bool { return e.What == "restarted (it was not responding)" }) {
		t.Errorf("history: %+v", v.History)
	}
}

// The robot checks what it gets: another manager's key, an old seq, another robot.
func TestChannelSignatures(t *testing.T) {
	m, base := serve(t, Config{})
	_, other := serve(t, Config{})
	const id = "stackchan-000000000002"
	r := setUpSim(t, base, id, `{"apps":["raw","pet"]}`)

	// A list signed by another manager for this robot: refused.
	_, setup := web(t, other, "POST", "/api/my/robots/"+id+"/setup", `{"apps":["raw"]}`)
	var s struct{ Manager struct{ Key string } }
	json.Unmarshal([]byte(setup), &s)
	m2 := newManager(t, Config{AppsFile: catalog(t), SigningKeyFile: filepath.Join(t.TempDir(), "k.pem")})
	m.mu.Lock()
	rb := m.robots[id]
	m2.robots[id] = &Robot{ID: id, Apps: rb.Apps, Seq: rb.Seq + 10, RemoteApps: true, Version: 99}
	forged, _ := m2.signLocked(m2.robots[id], m2.appsMsgLocked(m2.robots[id]))
	m.conns[id].sendFrame(forged)
	// The same list again (an old seq): refused.
	f, _ := m.signLocked(rb, m.appsMsgLocked(rb))
	m.conns[id].sendFrame(f)
	m.conns[id].sendFrame(f)
	m.mu.Unlock()
	eventually(t, "two refused", func() bool { return len(r.Snapshot().Refused) == 2 })
	if got := r.Snapshot().Refused; got[0] != "not signed by its manager" || got[1] != "old seq" {
		t.Errorf("refused: %v", got)
	}
	// A wrong channel token: no channel.
	req, _ := http.NewRequest("GET", strings.Replace(base, "http", "ws", 1)+"/robot", nil)
	req.Header.Set("X-Device-Id", id)
	req.Header.Set("Authorization", "Bearer nope")
	resp, err := http.DefaultClient.Do(req)
	if err == nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
}

// A robot set up for changes over USB only: nothing goes down the channel; the page says so.
func TestChannelRemoteOff(t *testing.T) {
	_, base := serve(t, Config{})
	const id = "stackchan-000000000003"
	setUpSim(t, base, id, `{"apps":["raw","pet"],"remote_apps":false}`)
	for _, c := range [][2]string{{"apps", `{"apps":["raw"]}`}, {"switch", `{"app":"pet"}`}} {
		if code, _ := web(t, base, "POST", "/api/my/robots/"+id+"/"+c[0], c[1]); code != http.StatusConflict {
			t.Errorf("%s: %d, want 409", c[0], code)
		}
	}
}

// A switch lost with a dropped channel is sent again when the robot is back soon.
func TestSwitchAfterADrop(t *testing.T) {
	_, base := serve(t, Config{})
	const id = "stackchan-000000000004"
	r := setUpSim(t, base, id, `{"apps":["raw","pet"],"start":"raw","ask_pin":false}`)
	r.LoseNextSigned()
	if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/switch", `{"app":"pet"}`); code != http.StatusOK {
		t.Fatalf("switch: %d %s", code, body)
	}
	eventually(t, "on Pet after the drop", func() bool {
		return r.Snapshot().Current == "wss://pet.example" && pageRobot(t, base, id).LastApp == "pet"
	})
}

// The manager is optional: turned off on the robot or from the page (never on from the page);
// then nothing changes the robot online; on again on the robot (or by a USB setup).
func TestManagerOff(t *testing.T) {
	_, base := serve(t, Config{})
	const id = "stackchan-000000000005"
	r := setUpSim(t, base, id, `{"apps":["raw","pet"],"start":"raw","ask_pin":false}`)

	// On the robot's Manager screen: off.
	r.TurnManager(false)
	eventually(t, "off on the page", func() bool { v := pageRobot(t, base, id); return v.Off == "robot" && !v.Online })
	for _, c := range [][2]string{{"switch", `{"app":"pet"}`}, {"apps", `{"apps":["raw"]}`}, {"restart", ""}} {
		if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/"+c[0], c[1]); code != http.StatusConflict || !strings.Contains(body, "manager_off") {
			t.Errorf("%s while off: %d %s", c[0], code, body)
		}
	}
	// On again on the robot: the page follows, changes work.
	r.TurnManager(true)
	eventually(t, "on again", func() bool { v := pageRobot(t, base, id); return v.Off == "" && v.Online })
	if code, _ := web(t, base, "POST", "/api/my/robots/"+id+"/switch", `{"app":"pet"}`); code != http.StatusOK {
		t.Errorf("switch after on: %d", code)
	}

	// From the page: off; the robot obeys and stays off.
	if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/disable", ""); code != http.StatusOK {
		t.Fatalf("disable: %d %s", code, body)
	}
	eventually(t, "off by the manager", func() bool { v := pageRobot(t, base, id); return r.ManagerOff() && v.Off == "manager" && !v.Online })
	time.Sleep(500 * time.Millisecond)
	if pageRobot(t, base, id).Online {
		t.Error("the robot came back on its own")
	}
	// A USB setup turns it on again.
	setUpSim(t, base, id, `{"apps":["raw","pet"],"start":"raw"}`)
	eventually(t, "on after a USB setup", func() bool { v := pageRobot(t, base, id); return v.Off == "" && v.Online })
}

// Turned off on the page while the robot is away: it gets it when it connects.
func TestManagerOffWhileAway(t *testing.T) {
	m, base := serve(t, Config{})
	const id = "stackchan-000000000006"
	code, res := web(t, base, "POST", "/api/my/robots/"+id+"/setup", `{"apps":["raw"]}`)
	if code != http.StatusOK {
		t.Fatal(res)
	}
	if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/disable", ""); code != http.StatusOK || !strings.Contains(body, `"pending":true`) {
		t.Fatalf("disable: %d %s", code, body)
	}
	if v := pageRobot(t, base, id); v.Off != "pending" {
		t.Errorf("page: %+v", v.Off)
	}
	r, _ := robotsim.FromSetup(id, []byte(res))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Run(ctx)
	eventually(t, "off when it came", func() bool { return r.ManagerOff() && pageRobot(t, base, id).Off == "manager" })
	_ = m
}

// A phone signs in to a home manager with the one-time code on a robot's Manager screen; the
// code works once. It signs out again.
func TestPhoneSignsInAtTheRobot(t *testing.T) {
	_, base := serve(t, Config{})
	const id = "stackchan-000000000007"
	r := setUpSim(t, base, id, `{"apps":["raw"]}`)
	eventually(t, "a page code on the robot", func() bool { return strings.Contains(r.PageURL(), "/phone?code=") })
	jar, _ := cookiejar.New(nil)
	phone := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(u string) (int, string) { // as a phone on the LAN: not this computer's host name
		req, _ := http.NewRequest("GET", u, nil)
		req.Host = "192.0.2.10:8790"
		resp, err := phone.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, me := get(base + "/api/me"); !strings.Contains(me, `"signed_in":false`) {
		t.Fatalf("a phone before: %s", me)
	}
	if code, _ := get(base + "/phone?code=nope"); code != http.StatusForbidden {
		t.Errorf("a wrong code: %d", code)
	}
	u, _ := url.Parse(r.PageURL())
	if code, _ := get(base + "/phone?" + u.RawQuery); code != http.StatusSeeOther {
		t.Fatalf("the robot's code: %d", code)
	}
	if _, me := get(base + "/api/me"); !strings.Contains(me, `"signed_in":true`) || !strings.Contains(me, `"phone":true`) || !strings.Contains(me, id) {
		t.Fatalf("the phone after: %s", me)
	}
	jar2, _ := cookiejar.New(nil)
	phone.Jar = jar2 // another phone with the same code
	if code, _ := get(base + "/phone?" + u.RawQuery); code != http.StatusForbidden {
		t.Errorf("the code twice: %d", code)
	}
	eventually(t, "a new code on the robot", func() bool { return r.PageURL() != "" && !strings.Contains(r.PageURL(), u.RawQuery) })

	// The first phone signs out.
	phone.Jar = jar
	logout := func(origin string) int {
		req, _ := http.NewRequest("POST", base+"/auth/logout", nil)
		req.Host = "192.0.2.10:8790"
		req.Header.Set("Origin", origin)
		resp, err := phone.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := logout("http://evil.example"); code != http.StatusForbidden {
		t.Errorf("a cross-origin sign-out: %d", code)
	}
	if _, me := get(base + "/api/me"); !strings.Contains(me, `"signed_in":true`) {
		t.Fatalf("signed out cross-origin: %s", me)
	}
	if code := logout("http://192.0.2.10:8790"); code != http.StatusNoContent {
		t.Fatalf("sign out: %d", code)
	}
	if _, me := get(base + "/api/me"); !strings.Contains(me, `"signed_in":false`) {
		t.Fatalf("the phone after signing out: %s", me)
	}
}

// S19: a USB setup with another manager: the robot tells its old manager on the way out, the
// old page says where it went and refuses changes; a USB setup back here clears it.
func TestRobotLeavesForAnotherManager(t *testing.T) {
	_, home := serve(t, Config{Name: "home"})
	_, other := serve(t, Config{Name: "other"})
	const id = "stackchan-000000000007"
	r := setUpSim(t, home, id, `{"apps":["raw","pet"],"start":"raw"}`)

	setUp := func(base string) {
		t.Helper()
		code, res := web(t, base, "POST", "/api/my/robots/"+id+"/setup", `{"apps":["raw"],"start":"raw"}`)
		if code != http.StatusOK {
			t.Fatalf("setup: %d %s", code, res)
		}
		if err := r.SetUp([]byte(res)); err != nil {
			t.Fatal(err)
		}
	}
	setUp(other)
	eventually(t, "online at the other", func() bool { return pageRobot(t, other, id).Online })
	eventually(t, "left on the old page", func() bool {
		v := pageRobot(t, home, id)
		return v.Left != nil && v.Left.To == "other" && !v.Online
	})
	if code, body := web(t, home, "POST", "/api/my/robots/"+id+"/switch", `{"app":"pet"}`); code != http.StatusConflict || !strings.Contains(body, `"left"`) {
		t.Errorf("switch after it left: %d %s", code, body)
	}
	if v := pageRobot(t, home, id); !slices.ContainsFunc(v.History, func(e Event) bool { return strings.Contains(e.What, "set up with other over USB") }) {
		t.Errorf("history: %+v", v.History)
	}

	// Back home over USB: the old note goes; the other manager now has it as left.
	setUp(home)
	eventually(t, "back home", func() bool { v := pageRobot(t, home, id); return v.Online && v.Left == nil })
	eventually(t, "left the other", func() bool { v := pageRobot(t, other, id); return v.Left != nil && v.Left.To == "home" })
}

// A catalog app marked e2e: the robot turns end-to-end encryption on for it, over USB and online.
func TestE2EFromTheCatalog(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	apps := `[{"id":"raw","name":"Raw data","url":"wss://raw.example","secret_file":"` + write("raw", "raw-secret") + `","e2e":true},` +
		`{"id":"pet","name":"Pet","url":"wss://pet.example","secret_file":"` + write("pet", "pet-secret") + `"}]`
	_, base := serve(t, Config{AppsFile: write("apps.json", apps)})
	const id = "stackchan-000000000008"
	r := setUpSim(t, base, id, `{"apps":["pet"],"start":"pet"}`)
	if r.E2E("wss://pet.example") {
		t.Fatal("pet is not marked e2e")
	}
	if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/apps", `{"apps":["pet","raw"],"start":"pet"}`); code != http.StatusOK {
		t.Fatalf("apps: %d %s", code, body)
	}
	eventually(t, "e2e on for raw online", func() bool { return r.E2E("wss://raw.example") })
	if r.E2E("wss://pet.example") {
		t.Error("pet turned e2e")
	}
	// Over USB too.
	r2 := setUpSim(t, base, "stackchan-000000000009", `{"apps":["raw","pet"],"start":"raw"}`)
	if !r2.E2E("wss://raw.example") {
		t.Error("USB setup: raw not e2e")
	}
}

// A robot that has a marked app without its encryption (set up before the mark reached it): on
// its next Hello the manager sends the list again, once.
func TestE2EResentToARobotWithout(t *testing.T) {
	m, base := serve(t, Config{})
	const id = "stackchan-00000000000a"
	r := setUpSim(t, base, id, `{"apps":["raw","pet"],"start":"raw"}`)
	if r.E2E("wss://raw.example") {
		t.Fatal("raw is not marked yet")
	}
	m.mu.Lock()
	for _, a := range m.apps {
		a.E2E = a.ID == "raw"
	}
	m.mu.Unlock()
	before := pageRobot(t, base, id).Version
	r.Reconnect()
	eventually(t, "e2e on after the Hello", func() bool { return r.E2E("wss://raw.example") })
	if r.E2E("wss://pet.example") {
		t.Error("pet turned e2e")
	}
	eventually(t, "the list applied", func() bool { v := pageRobot(t, base, id); return v.Online && v.Applied == v.Version })
	if v := pageRobot(t, base, id).Version; v != before+1 {
		t.Errorf("version %d, want one resend from %d", v, before)
	}
}
