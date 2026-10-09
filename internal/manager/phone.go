package manager

// A phone signs in to a home manager (no sign-in otherwise: only the computer it runs on) by
// scanning the QR code on a robot's Manager screen: being at the robot is the proof, as for
// pairing with an app. The manager sends each robot it is the primary of a one-time page code on
// its channel (PageCode); the robot shows <page>/phone?code=… as the Manager screen's QR. A code
// works once and for pageCodeTTL; then the robot gets a new one.

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"time"
)

const pageCodeTTL = 10 * time.Minute

type pageCode struct {
	code    string
	expires time.Time
}

// pageCodeLocked sends the robot a page code: a new one on hello, or when it ran out. m.mu held.
func (m *Manager) pageCodeLocked(rb *Robot, c *robotConn, hello bool) {
	if m.pageURL() == "" {
		return
	}
	if m.pageCodes == nil {
		m.pageCodes = map[string]pageCode{}
	}
	pc, ok := m.pageCodes[rb.ID]
	if !hello && ok && time.Now().Before(pc.expires) {
		return
	}
	pc = pageCode{code: randHex(12), expires: time.Now().Add(pageCodeTTL)}
	m.pageCodes[rb.ID] = pc
	f, _ := json.Marshal(map[string]any{"kind": "PageCode", "body": map[string]string{"url": m.pageURL() + "/phone?code=" + pc.code}})
	c.sendFrame(f)
}

// phoneSession: the browser is a phone signed in at a robot.
func (m *Manager) phoneSession(w http.ResponseWriter, r *http.Request) bool {
	s := m.session(w, r)
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.phones[s]
	return ok
}

// GET /phone?code=…: a phone scanned a robot's Manager screen.
func (m *Manager) handlePhone(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	session := m.session(w, r)
	m.mu.Lock()
	var rb *Robot
	for id, pc := range m.pageCodes {
		if code != "" && subtle.ConstantTimeCompare([]byte(code), []byte(pc.code)) == 1 && time.Now().Before(pc.expires) {
			rb = m.robots[id]
			delete(m.pageCodes, id)
		}
	}
	if rb != nil {
		if m.phones == nil {
			m.phones = map[string]time.Time{}
		}
		m.phones[session] = time.Now().UTC().Truncate(time.Second)
		m.noteLocked(rb, "robot", "a phone signed in to this manager at the robot")
		if c := m.conns[rb.ID]; c != nil {
			m.pageCodeLocked(rb, c, true) // the next one
		}
		m.requestSave()
	}
	m.mu.Unlock()
	if rb == nil {
		authPage(w, http.StatusForbidden, "Not signed in",
			"This code is used or expired. Open the robot's Manager screen again (the gear on its app switcher) and scan its code.")
		return
	}
	m.log.Info("phone signed in at a robot", "robot", rb.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// POST /auth/logout: a phone signs out (this computer stays the owner).
func (m *Manager) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	session := m.session(w, r)
	m.mu.Lock()
	if _, ok := m.phones[session]; ok {
		delete(m.phones, session)
		m.requestSave()
	}
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func authPage(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>%s</title><body style="font-family:system-ui;padding:16px"><h1>%s</h1><p>%s</p>`+
		`<p><a href="/">Back</a></p>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(text))
}
