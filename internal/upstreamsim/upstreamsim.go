// Package upstreamsim is an upstream manager (as sm.w42.eu is to a home manager) for tests: it
// approves a link (GET and POST /link/approve, POST /api/link/token) and speaks the upstream's
// side of the link (GET /link, home-w42-eu docs/manager-channel.md): it sends its Catalog, keeps
// what the home reports (Home, Robots, Robot), grants tokens of its apps (Grant, Revoke), answers
// Standby, and sends Requests, Pairings and Moved when the test says so.
package upstreamsim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// App is one of the upstream's apps.
type App struct {
	ID, Name, URL, Web string
	E2E                bool
}

// Upstream is a running upstream manager.
type Upstream struct {
	URL  string // http://127.0.0.1:…
	Name string

	srv  *httptest.Server
	apps []App

	mu      sync.Mutex
	codes   map[string]string // one-time code -> link token
	tokens  map[string]bool   // link tokens issued
	ws      *websocket.Conn
	home    map[string]any
	robots  map[string]map[string]any // robot id -> its view as the home sent it
	granted map[string]string         // robot/app -> token
	unpair  map[string][]string       // robot/app -> ids
	results map[string]chan Result
}

// Result is the home's answer to a Request.
type Result struct {
	Code int            `json:"code"`
	Body map[string]any `json:"body"`
}

// Start runs an upstream named name with these apps; Close ends it.
func Start(name string, apps ...App) *Upstream {
	u := &Upstream{Name: name, apps: apps, codes: map[string]string{}, tokens: map[string]bool{},
		robots: map[string]map[string]any{}, granted: map[string]string{}, unpair: map[string][]string{},
		results: map[string]chan Result{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /link/approve", u.approvePage)
	mux.HandleFunc("POST /link/approve", u.approve)
	mux.HandleFunc("POST /api/link/token", u.token)
	mux.HandleFunc("GET /link", u.link)
	u.srv = httptest.NewServer(mux)
	u.URL = u.srv.URL
	return u
}

// Close ends the upstream and its link.
func (u *Upstream) Close() {
	u.Drop()
	u.srv.Close()
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (u *Upstream) approvePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Link a home manager</title><h1>Link %s?</h1>`+
		`<form method="post" action="/link/approve"><input type="hidden" name="return" value="%s">`+
		`<input type="hidden" name="state" value="%s"><button>Link it</button></form>`,
		html.EscapeString(q.Get("name")), html.EscapeString(q.Get("return")), html.EscapeString(q.Get("state")))
}

func (u *Upstream) approve(w http.ResponseWriter, r *http.Request) {
	ret, err := url.Parse(r.FormValue("return"))
	if err != nil || ret.Host == "" {
		http.Error(w, "no return", http.StatusBadRequest)
		return
	}
	code := randHex(16)
	u.mu.Lock()
	u.codes[code] = randHex(32)
	u.mu.Unlock()
	q := ret.Query()
	q.Set("code", code)
	q.Set("state", r.FormValue("state"))
	ret.RawQuery = q.Encode()
	http.Redirect(w, r, ret.String(), http.StatusSeeOther)
}

func (u *Upstream) token(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code string }
	json.NewDecoder(r.Body).Decode(&req)
	u.mu.Lock()
	tok, ok := u.codes[req.Code]
	delete(u.codes, req.Code)
	if ok {
		u.tokens[tok] = true
	}
	u.mu.Unlock()
	if !ok {
		http.Error(w, "unknown code", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"home": "h1", "token": tok, "account": "Ema"})
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (u *Upstream) link(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	u.mu.Lock()
	ok := u.tokens[tok]
	u.mu.Unlock()
	if !ok {
		http.Error(w, "unknown link", http.StatusUnauthorized)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	u.mu.Lock()
	if u.ws != nil {
		u.ws.Close()
	}
	u.ws = ws
	catalog := []map[string]string{}
	for _, a := range u.apps {
		catalog = append(catalog, map[string]string{"id": a.ID, "name": a.Name, "web": a.Web})
	}
	u.sendLocked("Catalog", map[string]any{"apps": catalog})
	u.mu.Unlock()
	for {
		var f struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		if ws.ReadJSON(&f) != nil {
			break
		}
		u.handle(f.Kind, f.Body)
	}
	u.mu.Lock()
	if u.ws == ws {
		u.ws = nil
	}
	u.mu.Unlock()
}

func (u *Upstream) sendLocked(kind string, body any) error {
	if u.ws == nil {
		return errors.New("not linked")
	}
	u.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return u.ws.WriteJSON(map[string]any{"kind": kind, "body": body})
}

func (u *Upstream) handle(kind string, body json.RawMessage) {
	var b struct {
		ID, Robot, App string
		IDs            []string         `json:"ids"`
		Robots         []map[string]any `json:"robots"`
		Code           int              `json:"code"`
		Body           map[string]any   `json:"body"`
	}
	json.Unmarshal(body, &b)
	u.mu.Lock()
	defer u.mu.Unlock()
	switch kind {
	case "Home":
		json.Unmarshal(body, &u.home)
	case "Robots":
		u.robots = map[string]map[string]any{}
		for _, v := range b.Robots {
			id, _ := v["id"].(string)
			u.robots[id] = v
		}
	case "Robot":
		var v struct{ Robot map[string]any }
		if json.Unmarshal(body, &v) == nil && v.Robot != nil {
			id, _ := v.Robot["id"].(string)
			u.robots[id] = v.Robot
		}
	case "Grant":
		res := map[string]any{"id": b.ID, "robot": b.Robot, "app": b.App}
		res["error"] = "no app " + b.App
		for _, a := range u.apps {
			if a.ID == b.App {
				tok := randHex(32)
				u.granted[b.Robot+"/"+b.App] = tok
				delete(res, "error")
				res["name"], res["url"], res["web"], res["token"], res["e2e"] = a.Name, a.URL, a.Web, tok, a.E2E
			}
		}
		u.sendLocked("Granted", res)
	case "Standby":
		u.sendLocked("StandbyGranted", map[string]any{"id": b.ID, "robot": b.Robot, "key": "c3RhbmRieQ==",
			"name": u.Name, "url": strings.Replace(u.URL, "http://", "ws://", 1) + "/robot", "token": randHex(32),
			"page": u.URL, "remote_apps": true, "ask_pin": true})
	case "Revoke":
		delete(u.granted, b.Robot+"/"+b.App)
	case "Unpair":
		u.unpair[b.Robot+"/"+b.App] = append(u.unpair[b.Robot+"/"+b.App], b.IDs...)
	case "Result":
		if ch := u.results[b.ID]; ch != nil {
			delete(u.results, b.ID)
			ch <- Result{Code: b.Code, Body: b.Body}
		}
	}
}

// Linked: the home's link is open now.
func (u *Upstream) Linked() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.ws != nil
}

// Home is what the home said of itself last (name, mode, apps, local_url).
func (u *Upstream) Home() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.home
}

// Robot is the home's robot as the home reported it last (the page's view), nil if not.
func (u *Upstream) Robot(id string) map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.robots[id]
}

// Granted is the token granted for the robot's app, "" if none (or revoked).
func (u *Upstream) Granted(robot, app string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.granted[robot+"/"+app]
}

// Unpaired is what the home asked to unpair on the robot's app.
func (u *Upstream) Unpaired(robot, app string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.unpair[robot+"/"+app]
}

// Request asks the home to change its robot (kind: switch, restart, apps, pairings/remove,
// disable) and waits for its Result.
func (u *Upstream) Request(ctx context.Context, robot, kind string, body any) (Result, error) {
	id := randHex(8)
	ch := make(chan Result, 1)
	u.mu.Lock()
	u.results[id] = ch
	err := u.sendLocked("Request", map[string]any{"id": id, "robot": robot, "kind": kind, "body": body})
	u.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Pairings reports the browsers paired with the robot on one of the upstream's apps.
func (u *Upstream) Pairings(robot, app string, pairings []map[string]string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sendLocked("Pairings", map[string]any{"robot": robot, "app": app, "pairings": pairings})
}

// Moved tells the home that the robot made the upstream its primary on its screen (S16).
func (u *Upstream) Moved(robot string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sendLocked("Moved", map[string]any{"robot": robot})
}

// Drop closes the link (the upstream unreachable for a moment): the home dials again.
func (u *Upstream) Drop() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ws != nil {
		u.ws.Close()
		u.ws = nil
	}
}
