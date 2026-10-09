package manager

// Robots this manager is the primary of (home-w42-eu docs/manager-channel.md). At the USB setup
// the robot gets the manager's public key, its channel URL and a channel token; it keeps a
// connection to the manager (channel.go). Everything that changes the robot goes down that
// channel signed with the manager's key (ECDSA P-256 over SHA-256), for this robot, with a `seq`
// that only grows: the robot checks all three. Apps, Switch, Forget, Restart.
//
// New apps get a token; the manager keeps it in the clear (Robot.Pending) only until the robot
// has the list with it. Kept apps keep their tokens (the manager has only their hashes).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

type managedServer struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"` // only for apps the robot has no token for yet
	E2E   bool   `json:"e2e,omitempty"`   // the robot turns end-to-end encryption on for it
}

// signedMsg is the payload of a Signed frame: one kind, for one robot, from this manager.
type signedMsg struct {
	Robot   string `json:"robot"`
	Manager string `json:"manager"` // this manager's id (managerID)
	Kind    string `json:"kind"`    // Apps, Switch, Forget, Restart
	Seq     int32  `json:"seq"`

	// Apps
	Name    string          `json:"name,omitempty"` // the manager's name on the robot (it may change)
	Version int32           `json:"version,omitempty"`
	Servers []managedServer `json:"servers,omitempty"`
	Pin     string          `json:"pin,omitempty"` // the start app's URL
	// Switch
	App string `json:"app,omitempty"` // appID of the app's URL
	// Forget
	Browsers []string `json:"browsers,omitempty"`

	Issued time.Time `json:"issued"`
}

// SignedFrame is a Signed frame's body: base64 of the JSON payload, and the signature.
type SignedFrame struct {
	Payload string `json:"payload"`
	Sig     string `json:"sig"`
}

// appID is how the robot and the manager name an app without its URL: the first 8 bytes of
// SHA-256 of the URL, hex (the firmware computes the same).
func appID(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:8])
}

// loadOrCreateSigner reads the manager's P-256 key (PEM, PKCS#8), creating it if missing.
func loadOrCreateSigner(path string, create bool) (*ecdsa.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, fmt.Errorf("%s: no PEM", path)
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok || ec.Curve != elliptic.P256() {
			return nil, fmt.Errorf("%s: not a P-256 key", path)
		}
		return ec, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if !create {
		return nil, fmt.Errorf("%s is missing, and a new key is not made here (-create-signing-key=false): robots trust only the old one", path)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// publicKey is the manager's public key as robots get it: base64 of DER (SubjectPublicKeyInfo).
func (m *Manager) publicKey() string {
	if m.signer == nil {
		return ""
	}
	der, _ := x509.MarshalPKIXPublicKey(&m.signer.PublicKey)
	return base64.StdEncoding.EncodeToString(der)
}

// managerID is how robots know this manager: the first 12 hex characters of SHA-256 of its public
// key (DER), as the firmware computes it (managers.h).
func (m *Manager) managerID() string {
	if m.signer == nil {
		return ""
	}
	der, _ := x509.MarshalPKIXPublicKey(&m.signer.PublicKey)
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:6])
}

// managerName is this manager's name on the robot and the pages: Config.Name, or
// "home on <this computer>".
func (m *Manager) managerName() string {
	if m.cfg.Name != "" {
		return m.cfg.Name
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return "home on " + h
	}
	return "home"
}

// signLocked makes a Signed frame for the robot, with its next seq. m.mu held.
func (m *Manager) signLocked(rb *Robot, msg signedMsg) ([]byte, error) {
	if m.signer == nil {
		return nil, errors.New("no signing key")
	}
	rb.Seq++
	msg.Robot, msg.Manager, msg.Seq, msg.Issued = rb.ID, m.managerID(), rb.Seq, time.Now().UTC().Truncate(time.Second)
	b, _ := json.Marshal(msg)
	sum := sha256.Sum256(b)
	sig, err := ecdsa.SignASN1(rand.Reader, m.signer, sum[:])
	if err != nil {
		return nil, err
	}
	m.requestSave()
	return json.Marshal(map[string]any{"kind": "Signed", "body": SignedFrame{
		Payload: base64.StdEncoding.EncodeToString(b), Sig: base64.StdEncoding.EncodeToString(sig)}})
}

// appsMsgLocked is the robot's app list as a signed message. m.mu held.
func (m *Manager) appsMsgLocked(rb *Robot) signedMsg {
	msg := signedMsg{Kind: "Apps", Name: m.managerName(), Version: rb.Version, Servers: []managedServer{}}
	for _, app := range m.apps { // catalog order: the order on the robot's app switcher
		if _, ok := rb.Apps[app.ID]; !ok {
			continue
		}
		s := managedServer{Name: app.Name, URL: app.URL, Token: rb.Pending[app.ID], E2E: app.E2E}
		if app.TokenFile != "" {
			s.Token = app.token // a shared token: always known
		}
		msg.Servers = append(msg.Servers, s)
		if app.ID == rb.Start {
			msg.Pin = app.URL
		}
	}
	for _, id := range sortedKeys(rb.Upstream) { // apps of the linked sm.w42.eu (link.go)
		up := rb.Upstream[id]
		msg.Servers = append(msg.Servers, managedServer{Name: up.Name, URL: up.URL, Token: up.Token, E2E: up.E2E})
		if upstreamKey(id) == rb.Start {
			msg.Pin = up.URL
		}
	}
	return msg
}

// sendAppsLocked sends the robot its app list now, if it is connected and the manager may.
// m.mu held.
func (m *Manager) sendAppsLocked(rb *Robot) {
	if c := m.conns[rb.ID]; c != nil && rb.RemoteApps {
		if f, err := m.signLocked(rb, m.appsMsgLocked(rb)); err == nil {
			c.sendFrame(f)
		}
	}
}

func sortedKeys[V any](mp map[string]V) []string {
	keys := make([]string, 0, len(mp))
	for k := range mp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// upstreamKey is the page's id of a linked sm.w42.eu app ("up:<its id there>").
func upstreamKey(id string) string { return "up:" + id }

// robotAppIDs are the ids of the robot's apps for the page: catalog ids, and "up:<id>" for the
// linked sm.w42.eu's apps.
func robotAppIDs(rb *Robot) []string {
	ids := []string{}
	for id := range rb.Apps {
		ids = append(ids, id)
	}
	for id := range rb.Upstream {
		ids = append(ids, upstreamKey(id))
	}
	sort.Strings(ids)
	return ids
}

// appURLLocked is the URL of one of the robot's apps by page id ("" if it has no such app).
func (m *Manager) appURLLocked(rb *Robot, id string) string {
	if up, ok := strings.CutPrefix(id, "up:"); ok {
		if a := rb.Upstream[up]; a != nil {
			return a.URL
		}
		return ""
	}
	if _, ok := rb.Apps[id]; ok {
		if a := m.app(id); a != nil {
			return a.URL
		}
	}
	return ""
}

// POST /api/my/robots/{id}/apps {"apps": [...], "start": "<app id>"}: change the apps of a robot
// whose owner allowed it at the USB setup ("up:<id>" for the linked sm.w42.eu's apps). The robot
// gets the list at once when connected, else when it connects.
func (m *Manager) handleRobotApps(w http.ResponseWriter, r *http.Request) {
	m.robotAction(w, r, func(rb *Robot, body json.RawMessage) (int, map[string]any) {
		var req struct {
			Apps  []string `json:"apps"`
			Start string   `json:"start"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Apps) == 0 {
			return http.StatusBadRequest, map[string]any{"error": "bad_request", "message": `body must be {"apps": ["<app id>", …], "start": "<app id>"}`}
		}
		return m.changeAppsLocked(rb, req.Apps, req.Start, "online")
	})
}

// changeAppsLocked sets the robot's apps and start app and sends the list (how: "online", or
// "from sm.w42.eu" through the link). m.mu held.
func (m *Manager) changeAppsLocked(rb *Robot, apps []string, start, how string) (int, map[string]any) {
	if start == "" {
		start = apps[0]
	}
	if !slices.Contains(apps, start) {
		return http.StatusBadRequest, map[string]any{"error": "bad_start", "message": "the start app must be one of the apps"}
	}
	if code, res := m.offLocked(rb); code != 0 {
		return code, res
	}
	if m.signer == nil || !rb.RemoteApps {
		return http.StatusConflict, map[string]any{"error": "remote_off",
			"message": "this robot's apps can be changed only over USB (set up again to allow it)"}
	}
	var upstream []string
	for _, id := range apps {
		if up, ok := strings.CutPrefix(id, "up:"); ok {
			if !m.upstreamOfferedLocked(up) && rb.Upstream[up] == nil {
				return http.StatusBadRequest, map[string]any{"error": "no_app", "message": "no app " + id}
			}
			upstream = append(upstream, up)
		} else if m.app(id) == nil {
			return http.StatusBadRequest, map[string]any{"error": "no_app", "message": "no app " + id}
		}
	}
	now := time.Now()
	grants, pending := map[string]appGrant{}, map[string]string{}
	for _, id := range apps {
		if strings.HasPrefix(id, "up:") {
			continue
		}
		if g, kept := rb.Apps[id]; kept {
			grants[id] = g
			if t := rb.Pending[id]; t != "" {
				pending[id] = t
			}
			continue
		}
		app := m.app(id)
		hash := ""
		if app.TokenFile == "" && !app.RobotToken {
			token := randHex(32)
			sum := sha256.Sum256([]byte(token))
			hash = hex.EncodeToString(sum[:])
			pending[id] = token
		}
		grants[id] = appGrant{Hash: hash, Created: now}
	}
	var added, removed []string
	before := robotAppIDs(rb)
	for _, id := range apps {
		if !slices.Contains(before, id) {
			added = append(added, id)
		}
	}
	for _, id := range before {
		if !slices.Contains(apps, id) {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	what := "apps changed"
	for _, id := range added {
		what += " +" + id
	}
	for _, id := range removed {
		what += " −" + id
	}
	if start != rb.Start {
		what += ", starts with " + start
	}
	if how != "online" {
		what += " (" + how + ")"
	}
	rb.Apps, rb.Start, rb.Pending = grants, start, pending
	rb.Version++
	m.noteLocked(rb, "online", what)
	m.requestSave()
	// The linked sm.w42.eu's apps need their tokens first (link.go): the list goes when they come.
	if !m.setUpstreamAppsLocked(rb, upstream) {
		m.sendAppsLocked(rb)
	}
	return http.StatusOK, map[string]any{"ok": true, "version": rb.Version}
}

// POST /api/my/robots/{id}/switch {"app": "<app id>"}: the robot goes to that app now (asked on
// its screen when it was set up so). It answers on the channel (State): the page shows the
// question and the answer.
func (m *Manager) handleRobotSwitch(w http.ResponseWriter, r *http.Request) {
	m.robotAction(w, r, func(rb *Robot, body json.RawMessage) (int, map[string]any) {
		var req struct {
			App string `json:"app"`
		}
		if json.Unmarshal(body, &req) != nil || req.App == "" {
			return http.StatusBadRequest, map[string]any{"error": "bad_request", "message": `body must be {"app": "<app id>"}`}
		}
		return m.switchLocked(rb, req.App, "online")
	})
}

// switchLocked sends a signed Switch to one of the robot's apps. m.mu held.
func (m *Manager) switchLocked(rb *Robot, app, how string) (int, map[string]any) {
	u := m.appURLLocked(rb, app)
	if u == "" {
		return http.StatusBadRequest, map[string]any{"error": "no_app", "message": "not one of the robot's apps"}
	}
	if code, res := m.offLocked(rb); code != 0 {
		return code, res
	}
	c := m.conns[rb.ID]
	switch {
	case m.signer == nil || !rb.RemoteApps:
		return http.StatusConflict, map[string]any{"error": "remote_off",
			"message": "this robot switches apps only on its screen (set up again to allow the manager)"}
	case c == nil:
		return http.StatusConflict, map[string]any{"error": "offline", "message": "the robot is not connected to this manager"}
	}
	f, err := m.signLocked(rb, signedMsg{Kind: "Switch", App: appID(u)})
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": "sign", "message": err.Error()}
	}
	c.sendFrame(f)
	rb.State.Answer, rb.Asked = "", app
	rb.pending = &pendingSwitch{app: app, seq: rb.Seq, at: time.Now()}
	what := "asked to switch to " + app
	if how != "online" {
		what += " (" + how + ")"
	}
	m.noteLocked(rb, "online", what)
	return http.StatusOK, map[string]any{"ok": true, "ask": rb.AskPin}
}

// POST /api/my/robots/{id}/restart: the robot restarts (it comes back on its start app).
func (m *Manager) handleRobotRestart(w http.ResponseWriter, r *http.Request) {
	m.robotAction(w, r, func(rb *Robot, _ json.RawMessage) (int, map[string]any) {
		return m.restartLocked(rb, "online")
	})
}

func (m *Manager) restartLocked(rb *Robot, how string) (int, map[string]any) {
	if code, res := m.offLocked(rb); code != 0 {
		return code, res
	}
	c := m.conns[rb.ID]
	if c == nil {
		return http.StatusConflict, map[string]any{"error": "offline", "message": "the robot is not connected to this manager"}
	}
	f, err := m.signLocked(rb, signedMsg{Kind: "Restart"})
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": "sign", "message": err.Error()}
	}
	c.sendFrame(f)
	what := "restarted"
	if rb.State.Stuck {
		what = "restarted (it was not responding)"
	}
	if how != "online" {
		what += " (" + how + ")"
	}
	m.noteLocked(rb, "online", what)
	return http.StatusOK, map[string]any{"ok": true}
}

// robotAction runs fn for one of the owner's robots (POST, same origin, JSON body or none), with
// m.mu held.
func (m *Manager) robotAction(w http.ResponseWriter, r *http.Request, fn func(rb *Robot, body json.RawMessage) (int, map[string]any)) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if !m.owner(w, r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	var body json.RawMessage
	if r.ContentLength != 0 {
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil {
			http.Error(w, "body must be JSON", http.StatusBadRequest)
			return
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rb := m.robots[strings.ToLower(r.PathValue("id"))]
	if rb == nil {
		http.NotFound(w, r)
		return
	}
	code, res := fn(rb, body)
	if code == http.StatusOK {
		m.log.Info("robot action", "robot", rb.ID, "path", r.URL.Path)
	}
	writeJSON(w, code, res)
}

// pendingSwitch is a Switch sent and not applied yet: when the robot's channel drops and comes
// back soon, it is sent again (channel.go).
type pendingSwitch struct {
	app string
	seq int32
	at  time.Time
}

const resendSwitchFor = 30 * time.Second

// offLocked refuses changes to a robot whose manager is off (the answer's code, or 0). m.mu held.
func (m *Manager) offLocked(rb *Robot) (int, map[string]any) {
	if rb.Left != nil {
		return http.StatusConflict, map[string]any{"error": "left", "message": "this robot was set up with " + rb.Left.To +
			" over USB and talks only to that one now: set it up here again to manage it here"}
	}
	if rb.Off == "" && !rb.DisablePending {
		return 0, nil
	}
	return http.StatusConflict, map[string]any{"error": "manager_off", "message": "the manager is off on this robot: " +
		"its apps change only over USB and it switches apps on its app switcher; turn it on on the robot's Manager screen or with a USB setup"}
}

// POST /api/my/robots/{id}/disable: this manager turns itself off for the robot (a signed
// Disable): no channel, apps only over USB. It cannot turn itself on again: that is the robot's
// (its Manager screen) or a USB setup's.
func (m *Manager) handleRobotDisable(w http.ResponseWriter, r *http.Request) {
	m.robotAction(w, r, func(rb *Robot, _ json.RawMessage) (int, map[string]any) {
		return m.disableLocked(rb, "online")
	})
}

func (m *Manager) disableLocked(rb *Robot, how string) (int, map[string]any) {
	if rb.Off != "" {
		return http.StatusOK, map[string]any{"ok": true, "off": rb.Off}
	}
	rb.DisablePending = true
	m.sendDisableLocked(rb)
	what := "manager turned off: apps over USB only"
	if how != "online" {
		what += " (" + how + ")"
	}
	m.noteLocked(rb, "online", what)
	m.robotChangedLocked(rb)
	return http.StatusOK, map[string]any{"ok": true, "pending": m.conns[rb.ID] == nil}
}

// sendDisableLocked sends a pending Disable if the robot is connected. m.mu held.
func (m *Manager) sendDisableLocked(rb *Robot) {
	if c := m.conns[rb.ID]; c != nil && rb.DisablePending {
		if f, err := m.signLocked(rb, signedMsg{Kind: "Disable"}); err == nil {
			c.sendFrame(f)
		}
	}
}
