// Package robotsim is a robot's side of the manager channel (home-w42-eu
// docs/manager-channel.md), for tests and local runs: it does what the Embody Mode firmware does
// with the channel. It connects to its primary with the setup's channel URL and token, says Hello,
// checks every Signed frame (the primary's key, this robot, a growing seq) and applies Apps,
// Switch (asking "on its screen" when ask_pin: the test answers), Forget and Restart, and reports
// State.
package robotsim

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Manager is one of the robot's managers as the USB setup gave it.
type Manager struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Token      string `json:"token"`
	Page       string `json:"page"`
	AskPin     bool   `json:"ask_pin"`
	RemoteApps bool   `json:"remote_apps"`
	Version    int32  `json:"version"`
	Seq        int32  `json:"seq"`
	MayPrimary bool   `json:"may_primary"`
}

// App is one of the robot's apps.
type App struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
	E2E   bool   `json:"e2e,omitempty"`
}

// Robot is a simulated robot.
type Robot struct {
	ID       string
	Firmware string

	mu       sync.Mutex
	primary  Manager
	second   *Manager
	apps     []App
	pin      string
	current  string // the URL of the app it is on
	version  int32
	seq      int32
	question string
	answer   string
	stuck    bool
	forgot   []string
	restarts int
	refused  []string // why signed frames were refused
	ws       *websocket.Conn
	wake     chan struct{}

	// Answer decides a question on the screen; nil: nobody answers (it runs out).
	Answer func(question string) bool
	// LoseNext: the next Signed frame is lost with the connection (it drops at once).
	LoseNext bool
	off      bool            // the manager is off: no channel (Disable, or on its screen)
	pageURL  string          // its Manager screen's QR (PageCode)
	e2e      map[string]bool // URLs it encrypts for (on from a manager, never off)
}

// PageURL is the address its Manager screen's QR code shows now.
func (r *Robot) PageURL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pageURL
}

// New is a robot set up over USB: its apps, start app and managers (setup answer's fields).
func New(id string, apps []App, start string, primary Manager, second *Manager) *Robot {
	if !primary.RemoteApps && primary.Version == 0 {
		primary.RemoteApps = true
	}
	return &Robot{ID: id, Firmware: "1.5.1-mj41-sim", primary: primary, second: second, apps: apps, pin: start,
		current: start, version: primary.Version, seq: primary.Seq, wake: make(chan struct{}, 1)}
}

// FromSetup builds a robot from a manager's setup answer (POST /api/my/robots/{id}/setup).
func FromSetup(id string, setup []byte) (*Robot, error) {
	var s struct {
		Servers  []App    `json:"servers"`
		Start    string   `json:"start"`
		Manager  *Manager `json:"manager"`
		Manager2 *Manager `json:"manager2"`
	}
	if err := json.Unmarshal(setup, &s); err != nil {
		return nil, err
	}
	if s.Manager == nil {
		return nil, errors.New("no manager in the setup")
	}
	r := New(id, s.Servers, s.Start, *s.Manager, s.Manager2)
	for _, a := range s.Servers {
		r.e2eOnLocked(a)
	}
	return r, nil
}

// SetUp is a USB setup of the running robot (a manager's setup answer). With another manager than
// its primary it tells the old one first (Leaving), as the firmware does.
func (r *Robot) SetUp(setup []byte) error {
	n, err := FromSetup(r.ID, setup)
	if err != nil {
		return err
	}
	r.mu.Lock()
	ws, leaving := r.ws, r.primary.Key != n.primary.Key
	if leaving && ws != nil {
		b, _ := json.Marshal(map[string]any{"kind": "Leaving", "body": map[string]string{"to": n.primary.Name}})
		ws.WriteMessage(websocket.TextMessage, b)
	}
	r.primary, r.second, r.apps, r.pin, r.current = n.primary, n.second, n.apps, n.pin, n.current
	for _, a := range n.apps {
		r.e2eOnLocked(a)
	}
	r.version, r.seq, r.off = n.version, n.seq, false
	r.mu.Unlock()
	if ws != nil {
		ws.Close()
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return nil
}

// e2eOnLocked turns end-to-end encryption on for an app marked so (never off). r.mu held.
func (r *Robot) e2eOnLocked(a App) {
	if a.E2E {
		if r.e2e == nil {
			r.e2e = map[string]bool{}
		}
		r.e2e[a.URL] = true
	}
}

// Reconnect drops its channel; it comes back at once (a Hello).
func (r *Robot) Reconnect() {
	r.mu.Lock()
	ws := r.ws
	r.mu.Unlock()
	if ws != nil {
		ws.Close()
	}
}

// E2E: it encrypts for this app's URL.
func (r *Robot) E2E(url string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.e2e[url]
}

// Run keeps the channel to the primary up until ctx ends.
func (r *Robot) Run(ctx context.Context) {
	for ctx.Err() == nil {
		r.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (r *Robot) once(ctx context.Context) {
	r.mu.Lock()
	m, off := r.primary, r.off
	r.mu.Unlock()
	if off {
		return
	}
	h := http.Header{"Authorization": {"Bearer " + m.Token}, "X-Device-Id": {r.ID}}
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, m.URL, h)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.ws = ws
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.ws == ws {
			r.ws = nil
		}
		r.mu.Unlock()
		ws.Close()
	}()
	go func() { <-ctx.Done(); ws.Close() }()
	r.send("Hello", true)
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var f struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		if f.Kind == "PageCode" { // what its Manager screen's QR shows
			var b struct{ URL string }
			json.Unmarshal(f.Body, &b)
			r.mu.Lock()
			r.pageURL = b.URL
			r.mu.Unlock()
			continue
		}
		if f.Kind != "Signed" {
			continue
		}
		r.mu.Lock()
		lose := r.LoseNext
		r.LoseNext = false
		r.mu.Unlock()
		if lose {
			return // the connection drops with it
		}
		r.signed(f.Body)
	}
}

// send reports Hello or State.
func (r *Robot) send(kind string, hello bool) {
	r.mu.Lock()
	ws := r.ws
	st := map[string]any{"app": appID(r.current), "app_name": r.nameOf(r.current), "conn": "registered",
		"apps_version": r.version, "seq": r.seq, "stuck": r.stuck, "firmware": r.Firmware}
	if r.question != "" {
		st["question"] = map[string]any{"text": r.question, "seconds_left": 60}
	}
	if r.answer != "" {
		st["answer"] = r.answer
	}
	if true { // on Hello, and with every State (the firmware: whenever the list changed)
		apps := []map[string]any{}
		for _, a := range r.apps {
			apps = append(apps, map[string]any{"id": appID(a.URL), "name": a.Name, "e2e": r.e2e[a.URL]})
		}
		st["apps"] = apps
	}
	r.mu.Unlock()
	if ws == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"kind": kind, "body": st})
	r.mu.Lock()
	ws.WriteMessage(websocket.TextMessage, b)
	r.mu.Unlock()
}

func (r *Robot) nameOf(url string) string {
	for _, a := range r.apps {
		if a.URL == url {
			return a.Name
		}
	}
	return ""
}

func appID(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:8])
}

func (r *Robot) refuse(why string) {
	r.mu.Lock()
	r.refused = append(r.refused, why)
	r.mu.Unlock()
}

// signed checks and applies a Signed frame as the firmware does.
func (r *Robot) signed(body json.RawMessage) {
	var f struct{ Payload, Sig string }
	if json.Unmarshal(body, &f) != nil {
		return
	}
	payload, err1 := base64.StdEncoding.DecodeString(f.Payload)
	sig, err2 := base64.StdEncoding.DecodeString(f.Sig)
	r.mu.Lock()
	m := r.primary
	r.mu.Unlock()
	der, err3 := base64.StdEncoding.DecodeString(m.Key)
	if err1 != nil || err2 != nil || err3 != nil {
		r.refuse("not base64")
		return
	}
	k, err := x509.ParsePKIXPublicKey(der)
	pub, ok := k.(*ecdsa.PublicKey)
	sum := sha256.Sum256(payload)
	if err != nil || !ok || !ecdsa.VerifyASN1(pub, sum[:], sig) {
		r.refuse("not signed by its manager")
		return
	}
	var p struct {
		Robot    string `json:"robot"`
		Kind     string `json:"kind"`
		Seq      int32  `json:"seq"`
		Version  int32  `json:"version"`
		Servers  []App  `json:"servers"`
		Pin      string `json:"pin"`
		App      string `json:"app"`
		Browsers []string
	}
	if json.Unmarshal(payload, &p) != nil || p.Robot != r.ID {
		r.refuse("not for this robot")
		return
	}
	r.mu.Lock()
	if p.Seq <= r.seq {
		r.mu.Unlock()
		r.refuse("old seq")
		return
	}
	r.seq = p.Seq
	if p.Kind == "Disable" { // it may turn itself off, never on
		r.off = true
		ws := r.ws
		r.mu.Unlock()
		r.sendOff(ws, "manager")
		return
	}
	switch p.Kind {
	case "Apps":
		if !m.RemoteApps {
			r.mu.Unlock()
			r.refuse("remote changes off")
			return
		}
		next := []App{}
		for _, s := range p.Servers {
			if s.Token == "" {
				s.Token = r.tokenOf(s.URL)
			}
			if s.Token == "" {
				continue // as the firmware: an app without a token is skipped
			}
			next = append(next, s)
			r.e2eOnLocked(s)
		}
		r.apps, r.version = next, p.Version
		if r.nameOf(r.current) == "" { // the app it was on is gone
			r.current = p.Pin
		}
		r.pin = p.Pin
		r.mu.Unlock()
		r.send("State", false)
	case "Switch":
		target := ""
		for _, a := range r.apps {
			if appID(a.URL) == p.App {
				target = a.URL
			}
		}
		if target == "" {
			r.mu.Unlock()
			r.refuse("no such app")
			return
		}
		if !m.AskPin {
			r.current, r.answer = target, "switched"
			r.mu.Unlock()
			r.send("State", false)
			return
		}
		r.question, r.answer = "Connect to "+r.nameOf(target)+"?", ""
		answer := r.Answer
		r.mu.Unlock()
		r.send("State", false)
		go func() {
			yes := false
			if answer != nil {
				time.Sleep(50 * time.Millisecond)
				yes = answer("Connect to " + r.nameOf(target) + "?")
			}
			r.mu.Lock()
			r.question = ""
			if yes {
				r.current, r.answer = target, "switched"
			} else {
				r.answer = "not confirmed"
			}
			r.mu.Unlock()
			r.send("State", false)
		}()
	case "Forget":
		r.forgot = append(r.forgot, p.Browsers...)
		r.mu.Unlock()
		r.send("State", false)
	case "Restart":
		r.restarts++
		r.current, r.stuck, r.question, r.answer = r.pin, false, "", ""
		ws := r.ws
		r.mu.Unlock()
		if ws != nil {
			ws.Close() // it comes back with a Hello
		}
	default:
		r.mu.Unlock()
	}
}

func (r *Robot) tokenOf(url string) string {
	for _, a := range r.apps {
		if a.URL == url {
			return a.Token
		}
	}
	return ""
}

// SetStuck makes its app loop "stop" (or come back): the channel reports it.
func (r *Robot) sendOff(ws *websocket.Conn, by string) {
	if ws == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"kind": "Off", "body": map[string]string{"by": by}})
	r.mu.Lock()
	ws.WriteMessage(websocket.TextMessage, b)
	r.mu.Unlock()
	ws.Close()
}

// TurnManager is the person at the robot turning its manager off or on (Manager screen).
func (r *Robot) TurnManager(on bool) {
	r.mu.Lock()
	r.off = !on
	ws := r.ws
	r.mu.Unlock()
	if !on {
		r.sendOff(ws, "robot")
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// ManagerOff: the manager is off on the robot.
func (r *Robot) ManagerOff() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.off
}

// LoseNextSigned makes the next Signed frame get lost with the connection.
func (r *Robot) LoseNextSigned() {
	r.mu.Lock()
	r.LoseNext = true
	r.mu.Unlock()
}

func (r *Robot) SetStuck(on bool) {
	r.mu.Lock()
	r.stuck = on
	r.mu.Unlock()
	r.send("State", false)
}

// SwitchOnScreen is the person at the robot switching apps on its QR screen.
func (r *Robot) SwitchOnScreen(url string) {
	r.mu.Lock()
	r.current, r.answer = url, ""
	r.mu.Unlock()
	r.send("State", false)
}

// UseSecond is the person at the robot making its second manager the primary (S16): false if
// the setup did not allow it.
func (r *Robot) UseSecond() bool {
	r.mu.Lock()
	if r.second == nil || !r.second.MayPrimary {
		r.mu.Unlock()
		return false
	}
	old := r.primary
	old.MayPrimary, old.Seq = true, r.seq // it may come back the same way
	r.primary, r.second = *r.second, &old
	r.seq = r.primary.Seq
	r.version = 0 // the new manager's list is not on the robot: it sends it
	ws := r.ws
	r.mu.Unlock()
	if ws != nil {
		ws.Close()
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return true
}

// Snapshot is what the robot has now.
type Snapshot struct {
	Apps     []App
	Current  string
	Pin      string
	Version  int32
	Seq      int32
	Forgot   []string
	Restarts int
	Refused  []string
	Primary  string // its primary's name
	Online   bool
}

func (r *Robot) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{Apps: append([]App(nil), r.apps...), Current: r.current, Pin: r.pin, Version: r.version, Seq: r.seq,
		Forgot: append([]string(nil), r.forgot...), Restarts: r.restarts, Refused: append([]string(nil), r.refused...),
		Primary: r.primary.Name, Online: r.ws != nil}
}

// AppURL is the URL of its app with this name ("" if none).
func (r *Robot) AppURL(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.apps {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}
