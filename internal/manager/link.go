package manager

// The link between this home manager and an upstream manager, e.g. sm.w42.eu
// (home-w42-eu docs/manager-channel.md, S3, S4, S8, S14–S16). The home manager dials out
// (GET <upstream>/link with its link token), so NAT is no problem; frames are JSON {kind, body}
// both ways.
//
// The upstream shows the home's robots as the home reports them: read-only, or with full
// control, then its page's changes come down as Requests and this manager, the robot's primary,
// signs them. It also issues the tokens of its own apps for the home's robots (Grant) and a
// standby channel token (Standby, for S16), and passes down what its apps report of those
// robots' paired browsers.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	linkWait      = 10 * time.Second // a Request or Grant waits this long for its answer
	linkFrameSize = 256 << 10
)

// linkState is a home manager's link to sm.w42.eu (saved).
type linkState struct {
	Token      string         `json:"token,omitempty"`   // the link token (a credential: the state file is 0600)
	Home       string         `json:"home,omitempty"`    // this home's id there
	Account    string         `json:"account,omitempty"` // whose account it is linked to (for the page)
	Mode       string         `json:"mode,omitempty"`    // "read-only" or "full"
	ShareLocal bool           `json:"share_local,omitempty"`
	Catalog    []upCatalogApp `json:"catalog,omitempty"` // sm.w42.eu's apps
	Since      time.Time      `json:"since,omitzero"`

	Pending      string `json:"pending,omitempty"` // the state of a link being approved
	PendingMode  string `json:"pending_mode,omitempty"`
	PendingShare bool   `json:"pending_share,omitempty"`

	connected bool
	lastError string
}

// upCatalogApp is one of sm.w42.eu's apps as the home sees it.
type upCatalogApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Web  string `json:"web"`
}

// UpApp is a linked sm.w42.eu app granted to one of the home's robots, with its token (the home
// is the robot's primary: it puts the token in the robot's signed app list).
type UpApp struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Web   string `json:"web"`
	Token string `json:"token"`
	E2E   bool   `json:"e2e,omitempty"` // as its catalog says
}

type linkClient struct {
	ws   *websocket.Conn
	send chan []byte
	done chan struct{}
}

func (c *linkClient) sendMsg(kind string, body any) {
	f, _ := json.Marshal(map[string]any{"kind": kind, "body": body})
	select {
	case c.send <- f:
	case <-c.done:
	default:
	}
}

// linkWaiter is a request on the link waiting for its answer.
type linkWaiter chan json.RawMessage

func (m *Manager) newWaiterLocked() (string, linkWaiter) {
	if m.waiters == nil {
		m.waiters = map[string]linkWaiter{}
	}
	id := randHex(8)
	ch := make(linkWaiter, 1)
	m.waiters[id] = ch
	return id, ch
}

func (m *Manager) answerWaiter(id string, body json.RawMessage) {
	m.mu.Lock()
	ch := m.waiters[id]
	delete(m.waiters, id)
	m.mu.Unlock()
	if ch != nil {
		ch <- body
	}
}

func (m *Manager) wait(ctx context.Context, id string, ch linkWaiter) (json.RawMessage, error) {
	t := time.NewTimer(linkWait)
	defer t.Stop()
	select {
	case b := <-ch:
		return b, nil
	case <-t.C:
	case <-ctx.Done():
	}
	m.mu.Lock()
	delete(m.waiters, id)
	m.mu.Unlock()
	return nil, errors.New("no answer in time")
}

func wsURL(httpURL, path string) string {
	u := strings.TrimRight(httpURL, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://") + path
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://") + path
	}
	return u + path
}

// RunLink keeps the home's link to Config.UpstreamURL up while it has a link token, until ctx
// ends.
func (m *Manager) RunLink(ctx context.Context) {
	if m.cfg.UpstreamURL == "" {
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		m.mu.Lock()
		token := m.linkSt.Token
		m.mu.Unlock()
		if token == "" {
			select {
			case <-ctx.Done():
				return
			case <-m.linkWake:
			}
			continue
		}
		start := time.Now()
		err := m.linkOnce(ctx, token)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		m.mu.Lock()
		m.linkSt.connected, m.link = false, nil
		if err != nil {
			m.linkSt.lastError = err.Error()
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-m.linkWake:
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (m *Manager) linkOnce(ctx context.Context, token string) error {
	h := http.Header{"Authorization": {"Bearer " + token}}
	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL(m.cfg.UpstreamURL, "/link"), h)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			m.mu.Lock()
			m.linkSt.Token = "" // unlinked there
			m.requestSave()
			m.mu.Unlock()
			m.log.Warn("link refused: unlinked on the other side")
		}
		return err
	}
	ws.SetReadLimit(linkFrameSize)
	c := &linkClient{ws: ws, send: make(chan []byte, 64), done: make(chan struct{})}
	defer close(c.done)
	defer ws.Close()
	go linkWriter(ws, c.send, c.done)
	m.mu.Lock()
	m.link, m.linkSt.connected, m.linkSt.lastError = c, true, ""
	m.sendHomeLocked()
	m.sendRobotsLocked()
	m.mu.Unlock()
	m.log.Info("linked", "upstream", m.cfg.UpstreamURL)
	for {
		ws.SetReadDeadline(time.Now().Add(channelSilent))
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		var f struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		m.homeHandle(f.Kind, f.Body)
	}
}

func linkWriter(ws *websocket.Conn, send chan []byte, done chan struct{}) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	ping, _ := json.Marshal(map[string]any{"kind": "Ping", "body": map[string]any{}})
	for {
		var f []byte
		select {
		case <-done:
			return
		case f = <-send:
		case <-t.C:
			f = ping
		}
		ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if ws.WriteMessage(websocket.TextMessage, f) != nil {
			ws.Close()
			return
		}
	}
}

// homeHandle is the home's side of a frame from sm.w42.eu.
func (m *Manager) homeHandle(kind string, body json.RawMessage) {
	switch kind {
	case "Catalog":
		var b struct {
			Apps []upCatalogApp `json:"apps"`
		}
		if json.Unmarshal(body, &b) == nil {
			m.mu.Lock()
			m.linkSt.Catalog = b.Apps
			m.requestSave()
			m.mu.Unlock()
		}
	case "Granted", "StandbyGranted":
		var b struct {
			ID string `json:"id"`
		}
		json.Unmarshal(body, &b)
		if kind == "Granted" {
			m.homeGranted(body)
		}
		m.answerWaiter(b.ID, body)
	case "Pairings":
		var b struct {
			Robot    string    `json:"robot"`
			App      string    `json:"app"`
			Pairings []Pairing `json:"pairings"`
		}
		if json.Unmarshal(body, &b) == nil && b.Pairings != nil {
			m.mu.Lock()
			if rb := m.robots[b.Robot]; rb != nil && rb.Upstream[b.App] != nil {
				m.pairingsSeenLocked(rb, upstreamKey(b.App), b.Pairings)
				m.linkRobotLocked(rb)
				m.requestSave()
			}
			m.mu.Unlock()
		}
	case "Request":
		var b struct {
			ID    string          `json:"id"`
			Robot string          `json:"robot"`
			Kind  string          `json:"kind"`
			Body  json.RawMessage `json:"body"`
		}
		if json.Unmarshal(body, &b) != nil {
			return
		}
		code, res := m.homeRequest(b.Robot, b.Kind, b.Body)
		m.mu.Lock()
		if m.link != nil {
			m.link.sendMsg("Result", map[string]any{"id": b.ID, "code": code, "body": res})
		}
		m.mu.Unlock()
	case "Moved": // the robot made sm.w42.eu its primary on its screen (S16)
		var b struct {
			Robot string `json:"robot"`
		}
		if json.Unmarshal(body, &b) == nil {
			m.mu.Lock()
			if rb := m.robots[b.Robot]; rb != nil && rb.Moved == "" {
				rb.Moved = m.upstreamName() // its channel goes there now (it may come back: S16)
				m.noteLocked(rb, "robot", "made "+rb.Moved+" its manager (on its screen)")
				m.requestSave()
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) upstreamName() string {
	u, err := url.Parse(m.cfg.UpstreamURL)
	if err != nil || u.Host == "" {
		return m.cfg.UpstreamURL
	}
	return u.Host
}

// homeRequest runs a change sm.w42.eu's page asked for (full control only, S15).
func (m *Manager) homeRequest(robot, kind string, body json.RawMessage) (int, map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.linkSt.Mode != "full" {
		return http.StatusForbidden, map[string]any{"error": "read_only", "message": "this home allows " + m.upstreamName() + " to look only"}
	}
	rb := m.robots[robot]
	if rb == nil || rb.Moved != "" {
		return http.StatusNotFound, map[string]any{"error": "no_robot", "message": "no such robot at home"}
	}
	how := "from " + m.upstreamName()
	var req struct {
		App   string   `json:"app"`
		Apps  []string `json:"apps"`
		Start string   `json:"start"`
		ID    string   `json:"id"`
		All   bool     `json:"all"`
	}
	json.Unmarshal(body, &req)
	var code int
	var res map[string]any
	switch kind {
	case "switch":
		code, res = m.switchLocked(rb, req.App, how)
	case "restart":
		code, res = m.restartLocked(rb, how)
	case "apps":
		if len(req.Apps) == 0 {
			return http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "no apps"}
		}
		code, res = m.changeAppsLocked(rb, req.Apps, req.Start, how)
	case "pairings/remove":
		code, res = m.removePairingsLocked(rb, req.App, req.ID, req.All, how)
	case "disable":
		code, res = m.disableLocked(rb, how)
	default:
		return http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "unknown request " + kind}
	}
	m.log.Info("request through the link", "robot", robot, "kind", kind, "code", code)
	return code, res
}

// sendHomeLocked tells sm.w42.eu about this home. m.mu held.
func (m *Manager) sendHomeLocked() {
	if m.link == nil {
		return
	}
	apps := []appView{}
	for _, a := range m.apps { // names only: the home's addresses stay home unless shared
		v := appView{ID: a.ID, Name: a.Name}
		if m.linkSt.ShareLocal {
			v.Web = a.Web
		}
		apps = append(apps, v)
	}
	body := map[string]any{"name": m.managerName(), "mode": m.linkSt.Mode, "apps": apps}
	if m.linkSt.ShareLocal {
		body["local_url"] = m.pageURL()
	}
	m.link.sendMsg("Home", body)
}

// sendRobotsLocked sends all of the home's robots (the shadow). m.mu held.
func (m *Manager) sendRobotsLocked() {
	if m.link == nil {
		return
	}
	views := []robotView{}
	for _, id := range sortedKeys(m.robots) {
		if rb := m.robots[id]; rb.Moved == "" {
			views = append(views, m.shadowViewLocked(rb))
		}
	}
	m.link.sendMsg("Robots", map[string]any{"robots": views})
}

// shadowViewLocked is a robot as sm.w42.eu shows it: the page's view, without history. m.mu held.
func (m *Manager) shadowViewLocked(rb *Robot) robotView {
	v := m.robotViewLocked(rb)
	v.History = []Event{}
	return v
}

// linkRobotLocked sends one robot's new state up (a no-op when not linked). m.mu held.
func (m *Manager) linkRobotLocked(rb *Robot) {
	if m.link == nil || m.cfg.UpstreamURL == "" || rb.Moved != "" {
		return
	}
	m.link.sendMsg("Robot", map[string]any{"robot": m.shadowViewLocked(rb)})
}

// upstreamOfferedLocked: sm.w42.eu offers this app and the link is up. m.mu held.
func (m *Manager) upstreamOfferedLocked(id string) bool {
	return m.link != nil && slices.ContainsFunc(m.linkSt.Catalog, func(a upCatalogApp) bool { return a.ID == id })
}

// setUpstreamAppsLocked makes the robot's linked apps these: removed ones are revoked, new ones
// asked for (Grant). Returns true when it waits for grants: the app list goes to the robot when
// they come (homeGranted). m.mu held.
func (m *Manager) setUpstreamAppsLocked(rb *Robot, want []string) bool {
	for id := range rb.Upstream {
		if !slices.Contains(want, id) {
			delete(rb.Upstream, id)
			delete(rb.Pairings, upstreamKey(id))
			if m.link != nil {
				m.link.sendMsg("Revoke", map[string]any{"robot": rb.ID, "app": id})
			}
		}
	}
	waiting := false
	for _, id := range want {
		if rb.Upstream[id] == nil && m.link != nil {
			wid, _ := m.newWaiterLocked() // answered by homeGranted; nobody waits on the channel
			m.link.sendMsg("Grant", map[string]any{"id": wid, "robot": rb.ID, "app": id})
			waiting = true
		}
	}
	return waiting
}

// homeGranted stores a token sm.w42.eu issued for one of the home's robots and sends the robot
// its list.
func (m *Manager) homeGranted(body json.RawMessage) {
	var b struct {
		Robot, App, Name, URL, Web, Token, Error string
		E2E                                      bool
	}
	if json.Unmarshal(body, &b) != nil || b.Error != "" || b.URL == "" {
		m.log.Warn("grant refused upstream", "robot", b.Robot, "app", b.App, "err", b.Error)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rb := m.robots[b.Robot]
	if rb == nil {
		return
	}
	if rb.Upstream == nil {
		rb.Upstream = map[string]*UpApp{}
	}
	rb.Upstream[b.App] = &UpApp{Name: b.Name, URL: b.URL, Web: b.Web, Token: b.Token, E2E: b.E2E}
	m.requestSave()
	m.sendAppsLocked(rb)
	m.linkRobotLocked(rb)
}

// linkUnpairLocked asks sm.w42.eu to unpair browsers on one of its apps. m.mu held.
func (m *Manager) linkUnpairLocked(rb *Robot, app string, ids []string) {
	if m.link != nil {
		m.link.sendMsg("Unpair", map[string]any{"robot": rb.ID, "app": app, "ids": ids})
	}
}

// linkStandby asks sm.w42.eu for a standby channel for the robot (S16): its key, channel URL and
// token, for the USB setup.
func (m *Manager) linkStandby(ctx context.Context, robot string) (map[string]any, error) {
	m.mu.Lock()
	if m.link == nil {
		m.mu.Unlock()
		return nil, errors.New("not linked to " + m.upstreamName() + " now")
	}
	id, ch := m.newWaiterLocked()
	m.link.sendMsg("Standby", map[string]any{"id": id, "robot": robot})
	m.mu.Unlock()
	b, err := m.wait(ctx, id, ch)
	if err != nil {
		return nil, err
	}
	var res map[string]any
	if json.Unmarshal(b, &res) != nil || res["error"] != nil {
		return nil, fmt.Errorf("%v", res["error"])
	}
	delete(res, "id")
	res["may_primary"] = true
	return res, nil
}

// linkViewLocked is the link on the home's page. m.mu held.
func (m *Manager) linkViewLocked() map[string]any {
	v := map[string]any{"upstream": m.upstreamName(), "linked": m.linkSt.Token != "", "connected": m.linkSt.connected,
		"mode": m.linkSt.Mode, "share_local": m.linkSt.ShareLocal, "account": m.linkSt.Account}
	if m.linkSt.lastError != "" && !m.linkSt.connected {
		v["error"] = m.linkSt.lastError
	}
	return v
}

// POST /api/link/start {"mode": "read-only"|"full", "share_local": bool}: begin linking; the page
// goes to the returned URL on sm.w42.eu, where the owner approves.
func (m *Manager) handleLinkStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode       string `json:"mode"`
		ShareLocal bool   `json:"share_local"`
		Return     string `json:"return"` // this page's origin as the browser sees it
	}
	if !m.homeOwner(w, r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req) != nil {
		return
	}
	if req.Mode != "full" {
		req.Mode = "read-only"
	}
	ret := strings.TrimRight(req.Return, "/")
	if u, err := url.Parse(ret); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host {
		http.Error(w, "return must be this page's origin", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.linkSt.Pending, m.linkSt.PendingMode, m.linkSt.PendingShare = randHex(16), req.Mode, req.ShareLocal
	state := m.linkSt.Pending
	m.requestSave()
	m.mu.Unlock()
	q := url.Values{"name": {m.managerName()}, "return": {ret + "/link/done"}, "state": {state}}
	writeJSON(w, http.StatusOK, map[string]any{"url": strings.TrimRight(m.cfg.UpstreamURL, "/") + "/link/approve?" + q.Encode()})
}

// GET /link/done?code=…&state=…: back from sm.w42.eu; the code becomes the link token.
func (m *Manager) handleLinkDone(w http.ResponseWriter, r *http.Request) {
	if m.cfg.UpstreamURL == "" {
		http.NotFound(w, r)
		return
	}
	if !m.owner(w, r) {
		http.Error(w, "only on this computer", http.StatusForbidden)
		return
	}
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	m.mu.Lock()
	ok := state != "" && subtle.ConstantTimeCompare([]byte(state), []byte(m.linkSt.Pending)) == 1
	mode, share := m.linkSt.PendingMode, m.linkSt.PendingShare
	m.mu.Unlock()
	if !ok || code == "" {
		authPage(w, http.StatusBadRequest, "Not linked", "This link approval is not the one this manager started. Start again.")
		return
	}
	body, _ := json.Marshal(map[string]string{"code": code})
	req, _ := http.NewRequestWithContext(r.Context(), "POST", strings.TrimRight(m.cfg.UpstreamURL, "/")+"/api/link/token", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		authPage(w, http.StatusBadGateway, "Not linked", "Could not reach "+m.upstreamName()+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	var res struct {
		Home, Token, Account string
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<10)).Decode(&res) != nil || res.Token == "" {
		authPage(w, http.StatusBadGateway, "Not linked", m.upstreamName()+" did not accept the code ("+resp.Status+"). Start again.")
		return
	}
	m.mu.Lock()
	m.linkSt = linkState{Token: res.Token, Home: res.Home, Account: res.Account, Mode: mode, ShareLocal: share, Since: time.Now().UTC().Truncate(time.Second)}
	m.requestSave()
	m.mu.Unlock()
	m.wakeLink()
	m.log.Info("linked to upstream", "upstream", m.cfg.UpstreamURL, "mode", mode)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// POST /api/link/settings {"mode", "share_local"}: change what sm.w42.eu may do and see.
func (m *Manager) handleLinkSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode       string `json:"mode"`
		ShareLocal bool   `json:"share_local"`
	}
	if !m.homeOwner(w, r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req) != nil {
		return
	}
	if req.Mode != "full" {
		req.Mode = "read-only"
	}
	m.mu.Lock()
	m.linkSt.Mode, m.linkSt.ShareLocal = req.Mode, req.ShareLocal
	m.requestSave()
	m.sendHomeLocked()
	m.sendRobotsLocked()
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/link/unlink: forget the link here (sm.w42.eu drops the home when it next refuses it,
// or the owner removes it there).
func (m *Manager) handleUnlink(w http.ResponseWriter, r *http.Request) {
	if !m.homeOwner(w, r) {
		return
	}
	m.mu.Lock()
	if m.link != nil {
		m.link.sendMsg("Unlink", map[string]any{})
		m.link.ws.Close()
	}
	m.linkSt = linkState{}
	m.requestSave()
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// homeOwner checks a home-side link request: a home manager with an upstream, the local owner,
// same origin. It answers the request when not.
func (m *Manager) homeOwner(w http.ResponseWriter, r *http.Request) bool {
	switch ok := m.owner(w, r); {
	case m.cfg.UpstreamURL == "":
		http.NotFound(w, r)
	case !sameOrigin(r):
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
	case !ok:
		http.Error(w, "sign in first", http.StatusUnauthorized)
	default:
		return true
	}
	return false
}

func (m *Manager) wakeLink() {
	select {
	case m.linkWake <- struct{}{}:
	default:
	}
}
