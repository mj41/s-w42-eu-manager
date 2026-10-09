package manager

// The manager channel (home-w42-eu docs/manager-channel.md): every robot this manager is the
// primary of keeps a WebSocket to GET /robot, with its id (X-Device-Id) and its channel token
// (Authorization: Bearer) from the USB setup. The robot reports Hello and State frames; the
// manager sends Signed frames (managed.go) and a Ping every pingEvery. Frames are JSON {kind,
// body}, like the apps' wire protocol.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pingEvery     = 20 * time.Second
	channelSilent = 70 * time.Second // nothing from the robot this long: the connection is gone
	maxFrame      = 16 << 10
)

// RobotState is what the robot last told its manager (Hello, State).
type RobotState struct {
	App         string     `json:"app,omitempty"`      // appID of the app it is on
	AppName     string     `json:"app_name,omitempty"` // its name on the robot
	Conn        string     `json:"conn,omitempty"`     // connecting, registered, offline, rejected
	Question    *Question  `json:"question,omitempty"` // on its screen now
	Answer      string     `json:"answer,omitempty"`   // the last answer: switched, not confirmed, refused: …
	Stuck       bool       `json:"stuck,omitempty"`    // its app loop stopped (the channel still answers)
	AppsVersion int32      `json:"apps_version"`       // of the last app list it applied
	Seq         int32      `json:"seq"`                // of the last signed message it applied
	Firmware    string     `json:"firmware,omitempty"`
	Apps        []RobotApp `json:"apps,omitempty"` // Hello only: what it has
	// The camera and the microphone as set on the robot (only there or over USB): "on", "off",
	// "night 22:00-07:00"; and why they are off now, if they are.
	Privacy      string `json:"privacy,omitempty"`
	CameraMicOff string `json:"camera_mic_off,omitempty"`
}

// Question is a question on the robot's screen.
type Question struct {
	Text        string `json:"text"`
	SecondsLeft int    `json:"seconds_left"`
}

// RobotApp is one of the robot's apps as it reports them: no URL, no token.
type RobotApp struct {
	ID   string `json:"id"` // appID
	Name string `json:"name"`
	E2E  *bool  `json:"e2e,omitempty"` // end-to-end encrypted on the robot (nil: firmware before 0.5.4)
}

type robotConn struct {
	id   string
	ws   *websocket.Conn
	send chan []byte
	done chan struct{}
}

func (c *robotConn) sendFrame(f []byte) {
	select {
	case c.send <- f:
	case <-c.done:
	default: // full: the robot is too slow; the next Hello resends what matters
	}
}

var upgrader = websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }} // robots, not pages

// GET /robot: a robot's manager channel.
func (m *Manager) handleRobotChannel(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.Header.Get("X-Device-Id"))
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	m.mu.Lock()
	rb := m.robots[id]
	want := ""
	if rb != nil {
		want = rb.ChannelHash
	}
	m.mu.Unlock()
	wantB, err := hex.DecodeString(want)
	if err != nil || len(wantB) != sha256.Size {
		wantB = make([]byte, sha256.Size)
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(sum[:], wantB) != 1 || rb == nil || token == "" {
		http.Error(w, "unknown robot or token", http.StatusUnauthorized)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxFrame)
	c := &robotConn{id: id, ws: ws, send: make(chan []byte, 16), done: make(chan struct{})}
	m.mu.Lock()
	if old := m.conns[id]; old != nil {
		old.ws.Close() // the robot reconnected: the old connection is dead
	}
	m.conns[id] = c
	if rb.Moved != "" { // it made the linked manager its primary, and now this one again
		m.noteLocked(rb, "robot", "made this manager its manager again (on its screen)")
		rb.Moved = ""
	}
	m.mu.Unlock()
	m.log.Info("robot channel open", "robot", id, "remote", r.RemoteAddr)
	go m.channelWriter(c)
	defer func() {
		close(c.done)
		ws.Close()
		m.mu.Lock()
		if m.conns[id] == c {
			delete(m.conns, id)
			if rb := m.robots[id]; rb != nil {
				rb.LastSeen = time.Now().UTC().Truncate(time.Second)
				m.robotChangedLocked(rb)
			}
		}
		m.mu.Unlock()
		m.log.Info("robot channel closed", "robot", id)
	}()
	for {
		ws.SetReadDeadline(time.Now().Add(channelSilent))
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
		switch f.Kind {
		case "Hello", "State":
			var st RobotState
			if json.Unmarshal(f.Body, &st) != nil {
				continue
			}
			m.mu.Lock()
			if rb := m.robots[id]; rb != nil {
				m.robotStateLocked(rb, c, st, f.Kind == "Hello")
				m.pageCodeLocked(rb, c, f.Kind == "Hello")
			}
			m.mu.Unlock()
		case "Off": // the manager is off on the robot now; it closes the channel
			var b struct {
				By string `json:"by"`
			}
			json.Unmarshal(f.Body, &b)
			m.mu.Lock()
			if rb := m.robots[id]; rb != nil {
				rb.Off, rb.DisablePending = "manager", false
				if b.By == "robot" {
					rb.Off = "robot"
					m.noteLocked(rb, "robot", "manager turned off on the robot: apps over USB only")
				}
				m.robotChangedLocked(rb)
				m.requestSave()
			}
			m.mu.Unlock()
		case "Leaving": // a USB setup gives it another manager (S19); it closes the channel
			var b struct {
				To string `json:"to"`
			}
			json.Unmarshal(f.Body, &b)
			m.mu.Lock()
			if rb := m.robots[id]; rb != nil {
				m.leftLocked(rb, b.To)
			}
			m.mu.Unlock()
		case "Ping":
			m.mu.Lock()
			if rb := m.robots[id]; rb != nil {
				m.pageCodeLocked(rb, c, false) // a fresh one when it ran out
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) channelWriter(c *robotConn) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	ping, _ := json.Marshal(map[string]any{"kind": "Ping", "body": map[string]any{}})
	for {
		select {
		case <-c.done:
			return
		case f := <-c.send:
			c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if c.ws.WriteMessage(websocket.TextMessage, f) != nil {
				c.ws.Close()
				return
			}
		case <-t.C:
			c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if c.ws.WriteMessage(websocket.TextMessage, ping) != nil {
				c.ws.Close()
				return
			}
		}
	}
}

// robotStateLocked takes in what the robot reported, and on a Hello sends what it lacks: its app
// list, browsers to forget. m.mu held.
func (m *Manager) robotStateLocked(rb *Robot, c *robotConn, st RobotState, hello bool) {
	if st.Firmware != "" {
		rb.Firmware = cleanName(st.Firmware)
	} else {
		st.Firmware = rb.State.Firmware
	}
	if st.Apps == nil {
		st.Apps = rb.State.Apps // listed on Hello and when its list changed
	}
	if st.Answer != "" && st.Answer != rb.State.Answer {
		m.noteLocked(rb, "robot", answerText(st.Answer, st.AppName))
	}
	if st.Stuck && !rb.State.Stuck {
		m.noteLocked(rb, "robot", "not responding")
	}
	rb.State = st
	rb.LastSeen = time.Now().UTC().Truncate(time.Second)
	if st.Seq > rb.Seq { // the manager's state is older than the robot's: go on from the robot's
		rb.Seq = st.Seq
	}
	if st.AppsVersion >= rb.Version && len(rb.Pending) > 0 { // it has the tokens now
		rb.Pending = nil
	}
	if len(rb.Forget) > 0 && st.Seq >= rb.ForgetSeq && rb.ForgetSeq > 0 {
		rb.Forget, rb.ForgetSeq = nil, 0
	}
	if p := rb.pending; p != nil && st.Seq >= p.seq {
		rb.pending = nil // applied
	}
	if hello && rb.Left != nil { // back (only a USB setup here gives it a channel token again)
		rb.Left = nil
	}
	if hello && rb.Off != "" { // turned on again on the robot (or over USB)
		m.noteLocked(rb, "robot", "manager on again")
		rb.Off = ""
	}
	if hello && rb.DisablePending { // turned off on a page while the robot was away
		m.sendDisableLocked(rb)
	}
	if hello {
		// A switch lost with a dropped channel: once more, if it was asked for just now.
		if p := rb.pending; p != nil && time.Since(p.at) < resendSwitchFor {
			if u := m.appURLLocked(rb, p.app); u != "" {
				if f, err := m.signLocked(rb, signedMsg{Kind: "Switch", App: appID(u)}); err == nil {
					p.seq = rb.Seq
					c.sendFrame(f)
				}
			}
		}
		if rb.RemoteApps && st.AppsVersion < rb.Version {
			if f, err := m.signLocked(rb, m.appsMsgLocked(rb)); err == nil {
				c.sendFrame(f)
			}
		}
		if len(rb.Forget) > 0 {
			m.sendForgetLocked(rb)
		}
	}
	if st.Apps != nil && st.AppsVersion >= rb.Version {
		m.reconcileLocked(rb, c)
	}
	m.robotChangedLocked(rb)
	m.requestSave()
}

// reconcileLocked: the robot lacks some of this manager's apps although it has its latest list
// (another manager's list replaced it, then this one became its manager again; or it lost a
// token). Apps with a token of its own get a new one; the list goes again. An app whose token
// came from elsewhere (robot_token) cannot come back that way: noted once. m.mu held.
func (m *Manager) reconcileLocked(rb *Robot, c *robotConn) {
	if !rb.RemoteApps || m.signer == nil || rb.Off != "" || time.Since(rb.reconciled) < 30*time.Second {
		return
	}
	has, plain := map[string]bool{}, map[string]bool{}
	for _, a := range rb.State.Apps {
		has[a.ID] = true
		if a.E2E != nil && !*a.E2E && rb.e2eTried != rb.Version { // one try per list version
			plain[a.ID] = true
		}
	}
	renewed := []string{}
	for _, app := range m.apps {
		g, ok := rb.Apps[app.ID]
		if ok && app.E2E && plain[appID(app.URL)] { // marked e2e, not encrypted on the robot: the list says so
			renewed = append(renewed, app.ID)
			continue
		}
		if !ok || has[appID(app.URL)] {
			continue
		}
		switch {
		case app.RobotToken:
			if !g.Lost {
				g.Lost = true
				rb.Apps[app.ID] = g
				m.noteLocked(rb, "robot", app.ID+" is gone from the robot (its token came from another manager): set it up there again over USB")
			}
		case app.TokenFile != "":
			renewed = append(renewed, app.ID) // the shared token goes with the list anyway
		case rb.Pending[app.ID] == "":
			token := randHex(32)
			sum := sha256.Sum256([]byte(token))
			rb.Apps[app.ID] = appGrant{Hash: hex.EncodeToString(sum[:]), Created: time.Now()}
			if rb.Pending == nil {
				rb.Pending = map[string]string{}
			}
			rb.Pending[app.ID] = token
			renewed = append(renewed, app.ID)
		}
	}
	for id, up := range rb.Upstream {
		if !has[appID(up.URL)] || up.E2E && plain[appID(up.URL)] {
			renewed = append(renewed, upstreamKey(id)) // its token is known here: it goes again
		}
	}
	if len(renewed) == 0 {
		return
	}
	sort.Strings(renewed)
	rb.reconciled = time.Now()
	rb.Version++
	if len(plain) > 0 {
		rb.e2eTried = rb.Version
	}
	m.noteLocked(rb, "online", "apps sent again (the robot lacked "+strings.Join(renewed, ", ")+", or their end-to-end encryption)")
	if f, err := m.signLocked(rb, m.appsMsgLocked(rb)); err == nil {
		c.sendFrame(f)
	}
}

func answerText(answer, app string) string {
	switch answer {
	case "switched":
		return "switched to " + app
	case "not confirmed":
		return "a switch was not confirmed on the robot"
	}
	return answer
}

// robotChangedLocked tells whoever follows the robot: the pages (they poll) and, on a home
// manager, sm.w42.eu over the link (link.go). m.mu held.
func (m *Manager) robotChangedLocked(rb *Robot) {
	m.linkRobotLocked(rb)
}

// sendForgetLocked sends the robot the browsers to forget (if connected). m.mu held.
func (m *Manager) sendForgetLocked(rb *Robot) {
	c := m.conns[rb.ID]
	if c == nil || m.signer == nil {
		return
	}
	if f, err := m.signLocked(rb, signedMsg{Kind: "Forget", Browsers: rb.Forget}); err == nil {
		rb.ForgetSeq = rb.Seq
		c.sendFrame(f)
	}
}

// robotURL is the channel's URL for robots: Config.RobotURL, else from the public URL.
func (m *Manager) robotURL() string {
	if m.cfg.RobotURL != "" {
		return m.cfg.RobotURL
	}
	u := strings.TrimRight(m.cfg.PublicURL, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://") + "/robot"
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://") + "/robot"
	}
	return ""
}

// pageURL is the address of this manager's page for people (the robot's Manager screen).
func (m *Manager) pageURL() string {
	if m.cfg.PageURL != "" {
		return m.cfg.PageURL
	}
	return strings.TrimRight(m.cfg.PublicURL, "/")
}

// Left is a robot set up with another manager over USB (S19): it said so on its way out.
type Left struct {
	To string    `json:"to"` // the other manager's name
	At time.Time `json:"at"`
}

// leftLocked records that the robot left for another manager: nothing here reaches it any more,
// until a USB setup here. m.mu held.
func (m *Manager) leftLocked(rb *Robot, to string) {
	to = cleanName(to)
	if to == "" {
		to = "another manager"
	}
	rb.Left = &Left{To: to, At: time.Now().UTC().Truncate(time.Second)}
	rb.pending, rb.Asked = nil, ""
	m.noteLocked(rb, "robot", "set up with "+to+" over USB: it talks only to that one now")
	m.robotChangedLocked(rb)
	m.requestSave()
}
