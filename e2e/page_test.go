// Package e2e tests the manager's page in a real browser: headless Chrome against a manager with
// a test catalog, and a fake robot on Web Serial (fakeserial.js). It covers what needs no real
// robot (setting one up without flashing, writing apps, errors, changes online); flashing is
// tested on a robot (s-w42-eu-usb, the private notes' "Testing end to end").
//
//	go test ./e2e                 skipped without Chrome (CHROME=… to name it)
//	E2E_SHOTS=/tmp/shots go test ./e2e -v   also saves a screenshot of each step
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mj41/s-w42-eu-manager/internal/manager"
	"github.com/mj41/s-w42-eu-manager/internal/robotsim"
	"github.com/mj41/s-w42-eu-manager/internal/upstreamsim"
)

// --- the browser ---------------------------------------------------------------------------------

type chrome struct {
	t   *testing.T
	ws  *websocket.Conn
	mu  sync.Mutex
	id  int
	res map[int]chan json.RawMessage
}

func chromePath() string {
	if p := os.Getenv("CHROME"); p != "" {
		return p
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// startChrome runs a headless Chrome and opens a tab with the fake robot injected.
func startChrome(t *testing.T, robot map[string]string) *chrome {
	t.Helper()
	bin := chromePath()
	if bin == "" {
		t.Skip("no Chrome (set CHROME)")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	profile, _ := os.MkdirTemp("", "e2e-chrome-") // not t.TempDir: Chrome's helpers may still write there at the end
	cmd := exec.Command(bin, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
		fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir="+profile, "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		time.Sleep(200 * time.Millisecond)
		os.RemoveAll(profile)
	})
	var tab struct{ WebSocketDebuggerURL string }
	for i := 0; ; i++ {
		req, _ := http.NewRequest("PUT", fmt.Sprintf("http://127.0.0.1:%d/json/new?about:blank", port), nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			json.NewDecoder(resp.Body).Decode(&tab)
			resp.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("Chrome did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	ws, _, err := websocket.DefaultDialer.Dial(tab.WebSocketDebuggerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &chrome{t: t, ws: ws, res: map[int]chan json.RawMessage{}}
	go func() {
		for {
			var m struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if ws.ReadJSON(&m) != nil {
				return
			}
			c.mu.Lock()
			ch := c.res[m.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
		}
	}()
	fake, err := os.ReadFile("fakeserial.js")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(robot)
	c.call("Page.enable", nil)
	c.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.__fakeRobot = " + string(cfg) + ";\n" + string(fake)})
	c.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 620, "height": 1500, "deviceScaleFactor": 1, "mobile": false})
	return c
}

func (c *chrome) call(method string, params any) json.RawMessage {
	c.t.Helper()
	c.mu.Lock()
	c.id++
	id := c.id
	ch := make(chan json.RawMessage, 1)
	c.res[id] = ch
	c.ws.WriteJSON(map[string]any{"id": id, "method": method, "params": params})
	c.mu.Unlock()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		c.t.Fatalf("%s: no answer", method)
		return nil
	}
}

// eval runs JavaScript in the page and returns its value as JSON.
func (c *chrome) eval(js string) json.RawMessage {
	c.t.Helper()
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	json.Unmarshal(c.call("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true, "awaitPromise": true}), &r)
	return r.Result.Value
}

func (c *chrome) str(js string) string {
	var s string
	json.Unmarshal(c.eval(js), &s)
	return s
}

func (c *chrome) open(url string) {
	c.call("Page.navigate", map[string]any{"url": url})
	c.waitFor(`document.readyState === "complete" && !!document.querySelector("header .acct")`)
	time.Sleep(500 * time.Millisecond) // the page's first load and USB probe
}

// waitFor waits until the JavaScript condition is true (10 s).
func (c *chrome) waitFor(cond string) {
	c.t.Helper()
	for i := 0; i < 100; i++ {
		if string(c.eval("!!("+cond+")")) == "true" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.shot("timeout")
	c.t.Fatalf("waiting for %s; page error: %q", cond, c.str(`document.getElementById("err").textContent`))
}

// click presses the button (or link) with this text, in the card of robot (or anywhere).
func (c *chrome) click(text, in string) {
	c.t.Helper()
	scope := "document"
	if in != "" {
		scope = fmt.Sprintf("document.getElementById(%q)", "robot-"+in)
	}
	js := fmt.Sprintf(`(() => { const b = [...%s.querySelectorAll("button, a")].find((b) => b.textContent.trim() === %q); if (b) b.click(); return !!b; })()`, scope, text)
	if string(c.eval(js)) != "true" {
		c.shot("noclick")
		c.t.Fatalf("no button %q (in %q)", text, in)
	}
}

// untick unticks the checkbox whose label contains text.
func (c *chrome) untick(text string) {
	c.eval(fmt.Sprintf(`(() => { const l = [...document.querySelectorAll("label")].find((l) => l.textContent.includes(%q)); const i = l.querySelector("input"); i.checked = false; i.dispatchEvent(new Event("change")); })()`, text))
}

var shots = 0

func (c *chrome) shot(name string) {
	dir := os.Getenv("E2E_SHOTS")
	if dir == "" {
		return
	}
	var r struct{ Data string }
	json.Unmarshal(c.call("Page.captureScreenshot", map[string]any{"format": "png"}), &r)
	b, _ := base64.StdEncoding.DecodeString(r.Data)
	shots++
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d-%s-%s.png", shots, c.t.Name(), name)), b, 0o644)
}

// usbOps is what the page asked the fake robot, e.g. ["hello", "provision+manager", "restart"].
func (c *chrome) usbOps() []string {
	var reqs []map[string]any
	json.Unmarshal(c.eval(`window.__usbLog`), &reqs)
	var ops []string
	for _, r := range reqs {
		op, _ := r["op"].(string)
		if _, ok := r["manager"]; ok {
			op += "+manager"
		}
		if _, ok := r["wifi"]; ok {
			op += "+wifi"
		}
		ops = append(ops, op)
	}
	return ops
}

// --- the manager ---------------------------------------------------------------------------------

// newManager is a home manager (the page on this computer is the owner) with raw
// (token checked here), pet and sbot (a shared token); firmware only with withFirmware.
func newManager(t *testing.T, withFirmware bool) (*httptest.Server, *manager.Manager) {
	t.Helper()
	return newManagerWith(t, withFirmware, nil)
}

// newManagerWith is a manager with its config tweaked (a link, a name), on loopback; robots reach
// its channel at its own address, its link runs.
func newManagerWith(t *testing.T, withFirmware bool, tweak func(*manager.Config)) (*httptest.Server, *manager.Manager) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(v), 0o600)
		return p
	}
	apps, _ := json.Marshal([]map[string]any{
		{"id": "raw", "name": "Raw data", "url": "wss://raw.example", "secret_file": write("raw", "raw-secret"), "e2e": true},
		{"id": "pet", "name": "Pet", "url": "wss://pet.example", "secret_file": write("pet", "pet-secret")},
		{"id": "sbot", "name": "Sbot", "url": "ws://192.168.1.10:8780", "token_file": write("sbot", "shared")},
	})
	cfg := manager.Config{AppsFile: write("apps.json", string(apps)), StateFile: filepath.Join(dir, "state.json"),
		SigningKeyFile: filepath.Join(dir, "key.pem"), Name: "home on laptop", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if withFirmware {
		fw := filepath.Join(dir, "fw")
		os.MkdirAll(fw, 0o755)
		os.WriteFile(filepath.Join(fw, "app.bin"), []byte("not really firmware"), 0o644)
		os.WriteFile(filepath.Join(fw, "manifest.json"), []byte(`{"name":"Stackchan Embody Mode","version":"v9.9.9","chipFamily":"ESP32-S3","parts":[]}`), 0o644)
		cfg.FirmwareDir = fw
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.PublicURL = "http://" + l.Addr().String()
	if tweak != nil {
		tweak(&cfg)
	}
	m, err := manager.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// On loopback with a Host of 127.0.0.1, as the page on the manager's own computer.
	ts := httptest.NewUnstartedServer(m.Handler())
	ts.Listener.Close()
	ts.Listener = l
	ts.Start()
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.RunLink(ctx)
	return ts, m
}

type robotView struct {
	ID, Start, Firmware, Name string
	Apps                      []string
	RemoteApps                bool `json:"remote_apps"`
	Version, Applied          int32
}

func robots(t *testing.T, ts *httptest.Server) map[string]robotView {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var me struct{ Robots []robotView }
	json.NewDecoder(resp.Body).Decode(&me)
	out := map[string]robotView{}
	for _, r := range me.Robots {
		out[r.ID] = r
	}
	return out
}

// --- the tests -----------------------------------------------------------------------------------

const robotID = "stackchan-0a1b2c3d4e50"

// A new robot on the cable: the page notices it, sets it up without flashing (its firmware is the
// latest), with every app and remote changes allowed; its card shows a notice.
func TestSetUpANewRobot(t *testing.T) {
	ts, _ := newManager(t, false)
	c := startChrome(t, map[string]string{"id": robotID, "firmware": "v0.2.0"})
	c.open(ts.URL + "/")
	c.waitFor(`document.body.textContent.includes("A robot is plugged in: ` + robotID + `")`)
	c.shot("noticed")
	c.click("Set it up", "new")
	c.untick("Install the latest")
	c.shot("choices")
	c.click("Set up", "new")
	c.waitFor(`document.getElementById("robot-` + robotID + `")?.textContent.includes("written over USB")`)
	c.shot("done")
	if ops := strings.Join(c.usbOps(), ","); !strings.Contains(ops, "provision+manager") || !strings.HasSuffix(ops, "restart") {
		t.Errorf("USB: %s", ops)
	}
	// The catalog's e2e mark reaches the robot: Raw data encrypted, the others not.
	var reqs []map[string]any
	json.Unmarshal(c.eval(`window.__usbLog`), &reqs)
	e2e := map[string]bool{}
	for _, q := range reqs {
		if q["op"] == "provision" {
			for _, sv := range q["servers"].([]any) {
				m := sv.(map[string]any)
				e2e[m["url"].(string)] = m["e2e"] == true
			}
		}
	}
	if !e2e["wss://raw.example"] || e2e["wss://pet.example"] || len(e2e) != 3 {
		t.Errorf("e2e in the provision: %v", e2e)
	}
	r := robots(t, ts)[robotID]
	if len(r.Apps) != 3 || r.Start != "raw" || !r.RemoteApps || r.Version != 1 || r.Applied != 1 || r.Firmware != "v0.2.0" {
		t.Errorf("robot: %+v", r)
	}
}

// One of yours on the cable: "Write apps" writes them (no firmware, its Wi-Fi kept).
func TestWriteAppsOverUSB(t *testing.T) {
	ts, _ := newManager(t, false)
	setUp(t, ts, `{"apps":["raw"],"start":"raw"}`)
	c := startChrome(t, map[string]string{"id": robotID, "firmware": "v0.2.0"})
	c.open(ts.URL + "/")
	c.waitFor(`document.getElementById("robot-` + robotID + `")?.textContent.includes("On this computer's USB")`)
	c.click("Write apps", robotID)
	c.click("Write to the robot", robotID)
	c.waitFor(`document.getElementById("robot-` + robotID + `")?.textContent.includes("written over USB (apps)")`)
	c.shot("done")
	if ops := strings.Join(c.usbOps(), ","); strings.Contains(ops, "wifi") || !strings.Contains(ops, "provision+manager") {
		t.Errorf("USB: %s", ops)
	}
}

// A job that fails keeps its error on the page (the manager has no firmware to install).
func TestErrorStays(t *testing.T) {
	ts, _ := newManager(t, false)
	setUp(t, ts, `{"apps":["raw"]}`)
	c := startChrome(t, map[string]string{"id": robotID, "firmware": "v0.2.0"})
	c.open(ts.URL + "/")
	c.waitFor(`document.getElementById("robot-` + robotID + `")?.textContent.includes("On this computer's USB")`)
	c.click("Update firmware", robotID)
	c.untick("Back up")
	c.click("Write to the robot", robotID)
	c.waitFor(`document.querySelector(".panel .error")?.textContent.includes("has no firmware to install")`)
	time.Sleep(9 * time.Second) // the page looks at USB again after 8 s: the error must stay
	c.shot("error")
	if !strings.Contains(c.str(`document.querySelector(".panel .error")?.textContent || ""`), "has no firmware to install") {
		t.Error("the error went away")
	}
	if c.str(`document.querySelector("ol.steps li.fail")?.textContent || ""`) == "" {
		t.Error("no failed step marked")
	}
}

// Changes online: × on a chip removes the app, ★ makes another the start app; the version grows.
func TestChangeAppsOnline(t *testing.T) {
	ts, _ := newManager(t, false)
	setUp(t, ts, `{"apps":["raw","pet","sbot"],"start":"raw"}`)
	c := startChrome(t, map[string]string{"id": "stackchan-0000000000ff", "firmware": "v0.2.0"}) // another robot on the cable
	c.open(ts.URL + "/")
	card := `document.getElementById("robot-` + robotID + `")`
	chip := func(name, button string) string {
		return card + `.querySelector` + "(`.chip`) && [..." + card + `.querySelectorAll(".chip")].find((c) => c.firstChild.textContent === "` + name + `").querySelector("` + button + `").click()`
	}
	c.eval(chip("Sbot", "button:not(.star)")) // × on Sbot
	c.waitFor(card + `?.querySelectorAll(".chip").length === 2`)
	c.eval(chip("Pet", "button.star")) // ★ on Pet
	c.waitFor(card + `?.querySelector(".open a")?.textContent === "Open Pet"`)
	c.shot("changed")
	r := robots(t, ts)[robotID]
	if strings.Join(r.Apps, ",") != "pet,raw" || r.Start != "pet" || r.Version != 3 || r.Applied != 1 {
		t.Errorf("robot: %+v", r)
	}
	if !strings.Contains(c.str(card+`.textContent`), "The robot gets the change when it connects") {
		t.Error("no waiting line")
	}
}

// setUp adds the robot as a setup over USB would (the robot's id, the apps).
func setUp(t *testing.T, ts *httptest.Server, body string) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/my/robots/"+robotID+"/setup", strings.NewReader(body))
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: %v %v", err, resp.Status)
	}
	resp.Body.Close()
}

// Nobody taps Yes on the robot in time: the page says so and asks again with one button (only the
// last step: the firmware and the tokens are done).
func TestAskOnTheRobotAgain(t *testing.T) {
	ts, _ := newManager(t, false)
	setUp(t, ts, `{"apps":["raw","pet"],"start":"raw"}`)
	c := startChrome(t, map[string]string{"id": robotID, "firmware": "v0.2.0", "refuseOnce": "1"})
	c.open(ts.URL + "/")
	c.waitFor(`document.getElementById("robot-` + robotID + `")?.textContent.includes("On this computer's USB")`)
	c.click("Write apps", robotID)
	c.click("Write to the robot", robotID)
	c.waitFor(`document.querySelector(".panel .error")?.textContent.includes("Not confirmed on the robot")`)
	c.shot("not-confirmed")
	c.click("Ask on the robot again", robotID)
	c.waitFor(`document.getElementById("robot-` + robotID + `")?.textContent.includes("written over USB")`)
	c.shot("done")
	n := 0
	for _, op := range c.usbOps() {
		if strings.HasPrefix(op, "provision") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("provision sent %d times, want 2: %v", n, c.usbOps())
	}
}

// The browsers paired on an app, as it reports them: the owner opens "Paired browsers" and removes
// one; the app gets the removal in its next answer.
func TestRemoveAPairedBrowser(t *testing.T) {
	ts, _ := newManager(t, false)
	req, _ := http.NewRequest("POST", ts.URL+"/api/my/robots/"+robotID+"/setup", strings.NewReader(`{"apps":["raw","pet"],"start":"raw"}`))
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: %v", err)
	}
	var setup struct{ Servers []struct{ ID, Token string } }
	json.NewDecoder(resp.Body).Decode(&setup)
	resp.Body.Close()
	report := func() []string {
		t.Helper()
		body := `{"robot":"` + robotID + `","token":"` + setup.Servers[0].Token + `","seen":{"pairings":[` +
			`{"id":"aaaaaaaaaaaa","device":"Chrome on Android","paired":"2026-10-01T10:00:00Z","last_seen":"2026-10-04T10:00:00Z","e2e":"0011223344556677"},` +
			`{"id":"bbbbbbbbbbbb","device":"Firefox on Linux","watching":true}]}}`
		req, _ := http.NewRequest("POST", ts.URL+"/api/robot-auth", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer raw-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var ra struct{ Unpair []string }
		json.NewDecoder(resp.Body).Decode(&ra)
		return ra.Unpair
	}
	report()

	c := startChrome(t, map[string]string{"id": "stackchan-0000000000ff", "firmware": "v0.2.0"})
	c.open(ts.URL + "/")
	card := `document.getElementById("robot-` + robotID + `")`
	c.waitFor(card + `?.querySelector(".pairings summary")?.textContent === "Paired browsers: 2 (1 watching now)"`)
	c.eval(card + `.querySelector(".pairings summary").click()`)
	c.waitFor(card + `.querySelector(".pairings").open && ` + card + `.querySelector(".pairings").textContent.includes("Chrome on Android")`)
	c.shot("pairings")
	c.eval(`window.confirm = () => true`)
	c.eval(card + `.querySelector(".pairings li button").click()`) // the first: Chrome on Android
	c.waitFor(card + `?.querySelector(".pairings")?.textContent.includes("removed when the robot next uses Raw data")`)
	c.shot("removing")
	if c.str(`String(`+card+`.querySelector(".pairings").open)`) != "true" {
		t.Error("the list closed after the removal")
	}
	if got := report(); strings.Join(got, ",") != "aaaaaaaaaaaa" {
		t.Errorf("unpair %v", got)
	}
}

// setUpSim sets the robot up as a USB setup would and starts it (robotsim): it keeps its manager
// channel, like the firmware.
func setUpSim(t *testing.T, ts *httptest.Server, id, body string) *robotsim.Robot {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/my/robots/"+id+"/setup", strings.NewReader(body))
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	r, err := robotsim.FromSetup(id, b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Run(ctx)
	return r
}

// U6/S6, S9: the robot is on Pet; the owner opens Raw data (its start app): the robot asks on its
// screen, the page shows the question and then the answer. Later it hangs: Restart lights up.
func TestSwitchQuestionAndRestart(t *testing.T) {
	ts, _ := newManager(t, false)
	r := setUpSim(t, ts, robotID, `{"apps":["raw","pet"],"start":"raw"}`)
	c := startChrome(t, map[string]string{"id": "stackchan-0000000000ff", "firmware": "v0.2.0"})
	c.open(ts.URL + "/")
	card := `document.getElementById("robot-` + robotID + `")`
	c.waitFor(card + `?.textContent.includes("connected · on Raw data")`)
	r.SwitchOnScreen("wss://pet.example") // S7: on its screen; the page follows
	c.waitFor(card + `?.textContent.includes("connected · on Pet") && ` + card + `.textContent.includes("Switch to Raw data")`)
	release := make(chan struct{})
	r.Answer = func(string) bool { <-release; return true }
	c.click("Switch to Raw data", robotID)
	c.waitFor(card + `?.querySelector(".asking")?.textContent.startsWith("Tap Yes on the robot: “Connect to Raw data?”")`)
	c.shot("asking")
	close(release) // the person taps Yes
	c.waitFor(card + `?.textContent.includes("✓ On Raw data") && ` + card + `.textContent.includes("connected · on Raw data")`)
	c.shot("switched")

	// Restart: greyed out while it answers; it hangs: highlighted; restarted.
	if c.str(`String(`+card+`.querySelector(".open button").disabled)`) != "true" {
		t.Error("Restart enabled while the robot answers")
	}
	r.SetStuck(true)
	c.waitFor(card + `?.textContent.includes("not responding") && !` + card + `.querySelector(".open button").disabled`)
	c.shot("stuck")
	c.click("Restart the robot", robotID)
	c.waitFor(card + `?.textContent.includes("connected · on Raw data") && !` + card + `.textContent.includes("not responding")`)
	if s := r.Snapshot(); s.Restarts != 1 {
		t.Errorf("restarts: %d", s.Restarts)
	}
}

// homePair is a home manager with an upstream it may link to (upstreamsim, both on loopback),
// with the home's robot running.
func homePair(t *testing.T) (home *httptest.Server, up *upstreamsim.Upstream, r *robotsim.Robot) {
	t.Helper()
	up = upstreamsim.Start("sm.example", upstreamsim.App{ID: "raw", Name: "Raw data", URL: "wss://raw.example", Web: "https://raw.example"})
	t.Cleanup(up.Close)
	home, _ = newManagerWith(t, false, func(c *manager.Config) { c.UpstreamURL, c.Name = up.URL, "home on laptop" })
	r = setUpSim(t, home, robotID, `{"apps":["raw","pet"],"start":"raw"}`)
	return home, up, r
}

// S3, S14, S15 in the browser: the home page's "Link to …" goes to the upstream's approval and
// back; the link card then says what the upstream may do; full control and the local address
// are set on the card, and a switch from the upstream reaches the robot.
func TestLinkInTheBrowser(t *testing.T) {
	home, up, r := homePair(t)
	c := startChrome(t, map[string]string{"id": "stackchan-0000000000ff", "firmware": "v0.2.0"})
	c.open(home.URL + "/")
	c.waitFor(`document.getElementById("linkcard")?.textContent.includes("Link to 127.0.0.1")`)
	c.shot("link-offer")
	c.click("Link to 127.0.0.1:"+strings.Split(up.URL, ":")[2], "")
	c.waitFor(`location.pathname === "/link/approve" && document.body.textContent.includes("Link home on laptop?")`)
	c.shot("approve")
	c.eval(`document.querySelector("form button").click()`)
	c.waitFor(`location.origin === "` + home.URL + `" && document.getElementById("linkcard")?.textContent.includes("shows your robots there, read-only")`)
	c.waitFor(`document.getElementById("linkcard")?.querySelector(".dot.on")`)
	c.shot("linked")
	if h := up.Home(); h["name"] != "home on laptop" || h["mode"] != "read-only" || h["local_url"] != nil {
		t.Errorf("upstream: %+v", h)
	}

	// Full control and the local address, set at home.
	c.eval(`document.querySelector("#linkcard details").open = true`)
	c.eval(`document.querySelectorAll("#linkcard input")[0].click()`)
	c.waitFor(`document.getElementById("linkcard").textContent.includes("shows your robots there and may change them")`)
	c.eval(`document.querySelector("#linkcard details").open = true`)
	c.eval(`document.querySelectorAll("#linkcard input")[1].click()`)
	c.waitFor(`document.querySelectorAll("#linkcard input")[1]?.checked`)
	c.shot("settings")
	for i := 0; up.Home()["local_url"] != home.URL; i++ {
		if i > 50 {
			t.Fatalf("upstream: %+v", up.Home())
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.SwitchOnScreen("wss://pet.example")
	card := `document.getElementById("robot-` + robotID + `")`
	c.waitFor(card + `?.textContent.includes("Switch to Raw data")`)
	r.Answer = func(string) bool { return true }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if res, err := up.Request(ctx, robotID, "switch", map[string]string{"app": "raw"}); err != nil || res.Code != http.StatusOK {
		t.Fatalf("switch from upstream: %v %+v", err, res)
	}
	c.waitFor(card + `?.textContent.includes("connected · on Raw data")`)
	c.shot("switched-from-upstream")
	if s := r.Snapshot(); s.Current != "wss://raw.example" || len(s.Refused) != 0 {
		t.Errorf("robot: %+v", s)
	}
}

// S16 setup in the browser: with the link up, the USB panel offers the upstream as the second
// manager; the robot gets it with the provision.
func TestSetupWithSecondManager(t *testing.T) {
	home, _, _ := homePair(t)
	c := startChrome(t, map[string]string{"id": robotID, "firmware": "v0.2.0"})
	linkByAPI(t, home)
	c.open(home.URL + "/")
	c.waitFor(`document.getElementById("robot-` + robotID + `")?.textContent.includes("On this computer's USB")`)
	c.click("Write apps", robotID)
	c.waitFor(`document.body.textContent.includes("may become this robot's manager")`)
	c.eval(`[...document.querySelectorAll("label")].find((l) => l.textContent.includes("may become this robot's manager")).querySelector("input").click()`)
	c.shot("second")
	c.click("Write to the robot", robotID)
	c.waitFor(`document.getElementById("robot-` + robotID + `")?.textContent.includes("written over USB")`)
	var reqs []map[string]any
	json.Unmarshal(c.eval(`window.__usbLog`), &reqs)
	found := false
	for _, r := range reqs {
		if r["op"] == "provision" {
			m2, _ := r["manager2"].(map[string]any)
			found = m2 != nil && m2["may_primary"] == true && m2["name"] == "sm.example" && strings.HasSuffix(m2["url"].(string), "/robot")
		}
	}
	if !found {
		t.Errorf("no second manager in the provision: %v", c.usbOps())
	}
}

// linkByAPI links a home as the browser would (start, approve, done), without the browser.
func linkByAPI(t *testing.T, home *httptest.Server) {
	t.Helper()
	req, _ := http.NewRequest("POST", home.URL+"/api/link/start", strings.NewReader(`{"mode":"read-only","return":"`+home.URL+`"}`))
	req.Header.Set("Origin", home.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("link start: %v", err)
	}
	var start struct{ URL string }
	json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	u, _ := url.Parse(start.URL)
	q := u.Query()
	form := url.Values{"return": {q.Get("return")}, "state": {q.Get("state")}}
	resp, err = http.PostForm(u.Scheme+"://"+u.Host+"/link/approve", form) // follows back to /link/done, then the home page
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: %v %v", err, resp.Status)
	}
	resp.Body.Close()
	for i := 0; i < 50; i++ {
		resp, _ := http.Get(home.URL + "/api/me")
		var me struct{ Link struct{ Connected bool } }
		json.NewDecoder(resp.Body).Decode(&me)
		resp.Body.Close()
		if me.Link.Connected {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("not linked")
}

// The ⋯ menu closes on a click outside it (and on Escape), not only on ⋯ again.
func TestMenuClosesOutside(t *testing.T) {
	ts, _ := newManager(t, false)
	setUp(t, ts, `{"apps":["raw","pet"],"start":"raw"}`)
	c := startChrome(t, map[string]string{"id": "stackchan-0000000000ff", "firmware": "v0.2.0"})
	c.open(ts.URL + "/")
	menu := `document.querySelector("#robot-` + robotID + ` details.menu")`
	c.waitFor(menu)
	c.eval(menu + `.querySelector("summary").click()`)
	c.waitFor(menu + `.open`)
	c.eval(`document.querySelector("header").click()`)
	c.waitFor(`!` + menu + `.open`)
	c.eval(menu + `.querySelector("summary").click()`)
	c.waitFor(menu + `.open`)
	c.eval(`document.dispatchEvent(new KeyboardEvent("keydown", {key: "Escape"}))`)
	c.waitFor(`!` + menu + `.open`)
}

// The manager is optional: off on the robot, the card says so and offers nothing that changes
// it; on again on the robot; turned off from the ⋯ menu.
func TestManagerOffOnThePage(t *testing.T) {
	ts, _ := newManager(t, false)
	r := setUpSim(t, ts, robotID, `{"apps":["raw","pet"],"start":"raw"}`)
	c := startChrome(t, map[string]string{"id": "stackchan-0000000000ff", "firmware": "v0.2.0"})
	c.open(ts.URL + "/")
	card := `document.getElementById("robot-` + robotID + `")`
	c.waitFor(card + `?.textContent.includes("connected · on Raw data")`)
	r.TurnManager(false)
	c.waitFor(card + `?.textContent.includes("The manager is off on this robot (turned off on its screen)")`)
	c.shot("off-on-robot")
	if c.str(`String(!!`+card+`.querySelector(".chip button"))`) != "false" {
		t.Error("chip buttons while the manager is off")
	}
	r.TurnManager(true)
	c.waitFor(card + `?.textContent.includes("connected · on Raw data") && !` + card + `.textContent.includes("The manager is off")`)
	c.eval(`window.confirm = () => true`)
	c.eval(card + `.querySelector("details.menu summary").click()`)
	c.click("Turn the manager off (USB only)", robotID)
	c.waitFor(card + `?.textContent.includes("turned off from a page")`)
	c.shot("off-from-page")
	if !r.ManagerOff() {
		t.Error("the robot did not turn its manager off")
	}
}

// S19: the robot is set up with another manager over USB: it says so on its way out, and this
// page shows where it went instead of a robot that is just away; nothing here changes it.
func TestRobotLeftOnThePage(t *testing.T) {
	ts, _ := newManager(t, false)
	other, _ := newManagerWith(t, false, func(c *manager.Config) { c.Name = "sm.example" })
	r := setUpSim(t, ts, robotID, `{"apps":["raw","pet"],"start":"raw"}`)
	c := startChrome(t, map[string]string{"id": "stackchan-0000000000ff", "firmware": "v0.2.0"})
	c.open(ts.URL + "/")
	card := `document.getElementById("robot-` + robotID + `")`
	c.waitFor(card + `?.textContent.includes("connected · on Raw data")`)

	req, _ := http.NewRequest("POST", other.URL+"/api/my/robots/"+robotID+"/setup", strings.NewReader(`{"apps":["raw"],"start":"raw"}`))
	req.Header.Set("Origin", other.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("setup at the other: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := r.SetUp(b); err != nil {
		t.Fatal(err)
	}
	c.waitFor(card + `?.textContent.includes("Set up with sm.example over USB") && ` + card + `.textContent.includes("What it had when it left.")`)
	c.shot("left")
	if c.str(`String(!!`+card+`.querySelector(".chip button"))`) != "false" {
		t.Error("chip buttons for a robot that left")
	}
}

// Find a robot on USB: Chrome has not been allowed the port yet (a new computer), so nothing is
// noticed by itself; the button opens the chooser, and then: one of yours → its card with the USB
// strip; another robot → Add a robot with it; no answer as Embody Mode → the firmware first; the
// chooser closed → nothing.
func TestFindARobotOnUSB(t *testing.T) {
	ts, _ := newManager(t, false)
	setUpSim(t, ts, robotID, `{"apps":["raw","pet"],"start":"raw"}`)
	card := `document.getElementById("robot-` + robotID + `")`
	find := func(robot map[string]string) *chrome {
		t.Helper()
		c := startChrome(t, robot)
		c.open(ts.URL + "/")
		c.waitFor(card + ` && !document.getElementById("find").hidden && !document.getElementById("addrow").hidden`)
		if s := c.str(`String(!!` + card + `.querySelector(".usbstrip"))`); s != "false" {
			t.Errorf("a USB strip before the chooser: %s", s)
		}
		c.eval(`document.getElementById("find").click()`)
		return c
	}

	c := find(map[string]string{"id": robotID, "firmware": "v0.2.0", "ungranted": "1"})
	c.waitFor(card + `?.querySelector(".usbstrip") && ` + card + `.textContent.includes("Found on this computer's USB")`)
	c.shot("find-mine")

	c = find(map[string]string{"id": "stackchan-0000000000fe", "firmware": "v0.2.0", "ungranted": "1"})
	c.waitFor(`document.getElementById("robot-new")?.textContent.includes("A robot is plugged in: stackchan-0000000000fe")`)
	c.shot("find-other")

	c = find(map[string]string{"id": "x", "firmware": "", "ungranted": "1", "silent": "1"})
	c.waitFor(`document.getElementById("robot-new")?.textContent.includes("does not answer as Embody Mode")`)
	c.shot("find-silent")

	c = find(map[string]string{"id": robotID, "firmware": "v0.2.0", "ungranted": "1", "cancel": "1"})
	time.Sleep(1500 * time.Millisecond)
	if s := c.str(`String(!!document.getElementById("robot-new") || !!` + card + `.querySelector(".usbstrip"))`); s != "false" {
		t.Errorf("something opened after the chooser was closed: %s", s)
	}
}
