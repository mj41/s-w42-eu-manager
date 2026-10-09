// Package manager is a home's own Stackchan manager: on the computer it runs on (and on phones
// signed in at a robot) people add their robots, approve the apps each robot may use, and set
// robots up over USB with the official firmware and one token per approved app. Apps check a
// robot's token with the manager when it connects (POST /api/robot-auth). Design: home-w42-eu
// docs/stackchan-sites.md.
package manager

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mj41/s-w42-eu-raw/statestore"
)

//go:embed ui
var uiFS embed.FS

const (
	sessionCookie     = "sm_session"
	stateSaveInterval = 30 * time.Second
	authCacheHint     = time.Minute // how long an app may trust a robot-auth answer
)

var (
	robotIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	appIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	fileName       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

// Config of a manager.
type Config struct {
	PublicURL string // the URL browsers use (cookies are Secure behind TLS)
	StateFile string // "": in memory only
	// State: where the state goes instead of StateFile (statestore: a file, or Postgres).
	State statestore.Store
	UIDir string // development: serve the pages from this directory

	AppsFile string // the app catalog (JSON, see App)

	FirmwareDir, FirmwareRelease, FirmwareCacheDir string // the official firmware for the setup over USB

	SigningKeyFile string // the key that signs what robots get (managed.go), created if missing; "" = none
	// NoNewSigningKey: a missing SigningKeyFile is an error, not a reason for a new key (when the
	// key is provided: a new one would cut every robot off).
	NoNewSigningKey bool

	// The manager channel (channel.go): robots connect to RobotURL (default: from PublicURL, …/robot);
	// PageURL is this page's address shown on the robot's Manager screen (default PublicURL).
	Name              string // this manager's name for people and robots (default: managerName)
	RobotURL, PageURL string

	// The link (link.go): this home manager links up to UpstreamURL (e.g. https://sm.w42.eu).
	UpstreamURL string

	Log *slog.Logger
}

// App is an entry of the catalog. An app on the internet checks robot tokens with the manager:
// it has a SecretFile (its credential for /api/robot-auth), and every robot gets a token of its
// own for it. An app at home that accepts one shared robot token has a TokenFile instead, and
// every robot gets that token. An app another manager set the robot up for has RobotToken: the
// robot keeps the token it has (this manager never knows it).
type App struct {
	ID         string `json:"id"`            // "raw", "pet", …
	Name       string `json:"name"`          // shown on the robot's Manager screen and the pages
	URL        string `json:"url"`           // what robots connect to: wss://raw.sa.w42.eu
	Web        string `json:"web,omitempty"` // its page for people; default: url with http(s)://
	SecretFile string `json:"secret_file,omitempty"`
	TokenFile  string `json:"token_file,omitempty"`
	RobotToken bool   `json:"robot_token,omitempty"`
	// E2E: a relay whose app runs in the browser (Raw data): robots turn end-to-end encryption
	// on for it when they get it (home-w42-eu docs/e2ee.md §7). Never off from here.
	E2E bool `json:"e2e,omitempty"`

	secretHash [sha256.Size]byte
	token      string // the shared token (TokenFile)
}

// Robot is a robot added here, with the apps approved for it.
type Robot struct {
	ID        string              `json:"id"`
	Owner     string              `json:"owner"` // localOwner's (apps get it from /api/robot-auth)
	OwnerName string              `json:"owner_name"`
	Created   time.Time           `json:"created"`
	Public    bool                `json:"public,omitempty"` // its pairing code pairs anyone
	Start     string              `json:"start,omitempty"`  // the app it starts with
	Apps      map[string]appGrant `json:"apps"`             // app id -> its token's hash

	// This manager is its primary (managed.go, channel.go): the owner's choices at the USB setup,
	// the app list's version, the tokens of apps added since (until the robot has them), the
	// channel token's hash, the last seq of a signed message.
	RemoteApps  bool              `json:"remote_apps,omitempty"`
	AskPin      bool              `json:"ask_pin,omitempty"`
	Version     int32             `json:"version,omitempty"`
	Pending     map[string]string `json:"pending,omitempty"`
	ChannelHash string            `json:"channel,omitempty"`
	Seq         int32             `json:"seq,omitempty"`
	Upstream    map[string]*UpApp `json:"upstream,omitempty"` // apps of the linked sm.w42.eu (link.go)

	// For the owner's page (robotinfo.go).
	Name       string               `json:"name,omitempty"`
	Firmware   string               `json:"firmware,omitempty"`  // as it reported last (setup over USB, or the channel)
	LastSeen   time.Time            `json:"last_seen,omitempty"` // its channel was open
	State      RobotState           `json:"state"`               // what it reported last on the channel
	Asked      string               `json:"asked,omitempty"`     // the app the page last asked it to switch to
	pending    *pendingSwitch       // a Switch not applied yet (managed.go)
	reconciled time.Time            // its apps were sent again (channel.go)
	e2eTried   int32                // the list version sent again for end-to-end encryption (channel.go)
	pairingsAt map[string]time.Time // when each app last reported its pairings (pairings.go)
	History    []Event              `json:"history,omitempty"`
	Moved      string               `json:"moved,omitempty"` // on a home manager: sm.w42.eu became its primary (S16)
	Left       *Left                `json:"left,omitempty"`  // set up with another manager over USB (S19)
	// The manager is off on the robot ("robot": turned off on its Manager screen, "manager": from
	// a page): no channel, apps only over USB; on again only on the robot or by a USB setup.
	Off            string `json:"off,omitempty"`
	DisablePending bool   `json:"disable_pending,omitempty"` // turned off on a page, the robot not told yet

	// Browsers paired with it (pairings.go): by app as the apps report them, the ones removed here
	// until the app drops them, and the end-to-end ids the robot is to forget until it has the
	// signed message that says so.
	Pairings  map[string][]Pairing `json:"pairings,omitempty"`
	Unpair    map[string][]string  `json:"unpair,omitempty"`
	Forget    []string             `json:"forget,omitempty"`
	ForgetSeq int32                `json:"forget_seq,omitempty"`
}

type appGrant struct {
	Hash    string    `json:"sha256,omitempty"` // "" for a shared-token app
	Created time.Time `json:"created"`
	Lost    bool      `json:"lost,omitempty"` // a robot_token app the robot no longer has
}

// Manager is the HTTP service.
type Manager struct {
	cfg      Config
	log      *slog.Logger
	apps     []*App
	firmware *firmwareRelease
	signer   *ecdsa.PrivateKey // signs what robots get (managed.go); nil: changes only over USB
	link     *linkClient       // the home side of the link (link.go); nil: not linked

	mu        sync.Mutex
	robots    map[string]*Robot
	conns     map[string]*robotConn // robot id -> its open channel (channel.go)
	linkSt    linkState             // this home manager's link (link.go)
	linkWake  chan struct{}         // a new link token: dial now
	waiters   map[string]linkWaiter // link requests waiting for their answer
	phones    map[string]time.Time  // session -> signed in at a robot (phone.go)
	pageCodes map[string]pageCode   // robot id -> the code its Manager screen shows (phone.go)

	saveNow   chan struct{}
	saveMu    sync.Mutex
	lastSaved []byte
}

// New loads the catalog and the state.
func New(cfg Config) (*Manager, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	m := &Manager{cfg: cfg, log: cfg.Log, robots: map[string]*Robot{}, conns: map[string]*robotConn{},
		linkWake: make(chan struct{}, 1), saveNow: make(chan struct{}, 1)}
	if cfg.FirmwareDir == "" && cfg.FirmwareRelease != "" {
		m.firmware = newFirmwareRelease("https://github.com/mj41/StackChan/releases", cfg.FirmwareRelease, cfg.FirmwareCacheDir)
	}
	if cfg.AppsFile != "" {
		apps, err := LoadApps(cfg.AppsFile)
		if err != nil {
			return nil, err
		}
		m.apps = apps
	}
	if cfg.SigningKeyFile != "" {
		k, err := loadOrCreateSigner(cfg.SigningKeyFile, !cfg.NoNewSigningKey)
		if err != nil {
			return nil, fmt.Errorf("signing key: %w", err)
		}
		m.signer = k
	}
	if m.cfg.State == nil && cfg.StateFile != "" {
		m.cfg.State = &statestore.File{Path: cfg.StateFile}
	}
	if err := m.loadState(); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadApps reads the catalog: a JSON list of App. Secret and token files are read now.
func LoadApps(path string) ([]*App, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var apps []*App
	if err := json.Unmarshal(b, &apps); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, a := range apps {
		switch {
		case !appIDPattern.MatchString(a.ID) || seen[a.ID]:
			return nil, fmt.Errorf("%s: app id %q: lowercase letters, digits and -, unique", path, a.ID)
		case !strings.HasPrefix(a.URL, "ws://") && !strings.HasPrefix(a.URL, "wss://"):
			return nil, fmt.Errorf("%s: app %s: url must start with ws:// or wss://", path, a.ID)
		case a.RobotToken && (a.SecretFile != "" || a.TokenFile != ""):
			return nil, fmt.Errorf("%s: app %s: robot_token (the robot keeps its own) takes no secret_file or token_file", path, a.ID)
		case !a.RobotToken && (a.SecretFile == "") == (a.TokenFile == ""):
			return nil, fmt.Errorf("%s: app %s: give secret_file (it checks tokens with the manager), token_file (a shared token) or robot_token", path, a.ID)
		}
		seen[a.ID] = true
		if a.Name == "" {
			a.Name = a.ID
		}
		if a.Web == "" {
			a.Web = "http" + strings.TrimPrefix(a.URL, "ws") // ws:// -> http://, wss:// -> https://
		}
		if !strings.HasPrefix(a.Web, "http://") && !strings.HasPrefix(a.Web, "https://") {
			return nil, fmt.Errorf("%s: app %s: web must start with http:// or https://", path, a.ID)
		}
		if a.RobotToken {
			continue
		}
		file := a.SecretFile + a.TokenFile
		v, err := os.ReadFile(file)
		if err != nil || strings.TrimSpace(string(v)) == "" {
			return nil, fmt.Errorf("%s: app %s: %s: %v", path, a.ID, file, err)
		}
		if a.SecretFile != "" {
			a.secretHash = sha256.Sum256([]byte(strings.TrimSpace(string(v))))
		} else {
			a.token = strings.TrimSpace(string(v))
		}
	}
	return apps, nil
}

func (m *Manager) app(id string) *App {
	for _, a := range m.apps {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// Handler serves the pages and the API.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { m.servePage(w, r, "index.html") })
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", http.StatusMovedPermanently) })
	mux.HandleFunc("GET /usb.js", m.handleUSBScript)
	mux.HandleFunc("GET /vendor/{file}", m.handleVendor)
	mux.HandleFunc("GET /firmware/{file}", m.handleFirmware)
	mux.HandleFunc("POST /auth/logout", m.handleLogout)
	mux.HandleFunc("GET /api/me", m.handleMe)
	mux.HandleFunc("POST /api/my/robots/{id}/setup", m.handleRobotSetup)
	mux.HandleFunc("POST /api/my/robots/{id}/access", m.handleRobotAccess)
	mux.HandleFunc("POST /api/my/robots/{id}/apps", m.handleRobotApps)
	mux.HandleFunc("POST /api/my/robots/{id}/name", m.handleRobotName)
	mux.HandleFunc("POST /api/my/robots/{id}/switch", m.handleRobotSwitch)
	mux.HandleFunc("POST /api/my/robots/{id}/restart", m.handleRobotRestart)
	mux.HandleFunc("POST /api/my/robots/{id}/disable", m.handleRobotDisable)
	mux.HandleFunc("GET /robot", m.handleRobotChannel)
	mux.HandleFunc("GET /phone", m.handlePhone)
	if m.cfg.UpstreamURL != "" { // the link (link.go)
		mux.HandleFunc("POST /api/link/start", m.handleLinkStart)
		mux.HandleFunc("GET /link/done", m.handleLinkDone)
		mux.HandleFunc("POST /api/link/settings", m.handleLinkSettings)
		mux.HandleFunc("POST /api/link/unlink", m.handleUnlink)
	}
	mux.HandleFunc("POST /api/my/robots/{id}/pairings/remove", m.handleRemovePairings)
	mux.HandleFunc("DELETE /api/my/robots/{id}", m.handleRemoveRobot)
	mux.HandleFunc("POST /api/robot-auth", m.handleRobotAuth)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

// --- sessions and the owner -------------------------------------------------------------------

func (m *Manager) session(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && len(c.Value) == 64 {
		if _, err := hex.DecodeString(c.Value); err == nil {
			return c.Value
		}
	}
	id := randHex(32)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || strings.HasPrefix(m.cfg.PublicURL, "https://")})
	return id
}

// localOwner owns every robot added here: whoever uses the page on the manager's own computer
// (loopback, no proxy in between), or a phone signed in at a robot (phone.go).
var localOwner = struct{ Key, Name string }{Key: "local", Name: "this computer"}

// owner: the request may act as the owner (this computer, or a phone signed in at a robot).
func (m *Manager) owner(w http.ResponseWriter, r *http.Request) bool {
	return isLocal(r) || m.phoneSession(w, r)
}

// isLocal: the request comes from this computer to this computer, with no proxy in between.
func isLocal(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	loop := func(hostport string) bool {
		host := hostport
		if h, _, err := net.SplitHostPort(hostport); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.IsLoopback()
	}
	return loop(r.RemoteAddr) && loop(r.Host)
}

// sameOrigin is the CSRF check for requests that change something.
func sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && u.Host != "" && u.Host == r.Host
}

// --- pages -------------------------------------------------------------------------------------

// GET /usb.js: the page's module for USB (Web Serial, flashing, backups).
func (m *Manager) handleUSBScript(w http.ResponseWriter, r *http.Request) {
	b, err := uiFS.ReadFile("ui/usb.js")
	if m.cfg.UIDir != "" {
		b, err = os.ReadFile(filepath.Join(m.cfg.UIDir, "usb.js"))
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (m *Manager) servePage(w http.ResponseWriter, r *http.Request, name string) {
	m.session(w, r)
	page, err := uiFS.ReadFile("ui/" + name)
	if m.cfg.UIDir != "" {
		page, err = os.ReadFile(filepath.Join(m.cfg.UIDir, name))
	}
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(page)
}

func (m *Manager) handleVendor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	b, err := uiFS.ReadFile("ui/vendor/" + name)
	if !fileName.MatchString(name) || !strings.HasSuffix(name, ".js") || err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

// handleFirmware (GET /firmware/{file}): the official firmware for the setup over USB, from this origin
// (browsers cannot fetch GitHub release files: no CORS); for whoever may set robots up.
func (m *Manager) handleFirmware(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !fileName.MatchString(name) || (m.cfg.FirmwareDir == "" && m.firmware == nil) {
		http.NotFound(w, r)
		return
	}
	if !m.owner(w, r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if m.cfg.FirmwareDir == "" {
		if err := m.firmware.serve(w, r, name); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				m.log.Warn("firmware release", "file", name, "err", err)
			}
			http.Error(w, "the firmware release is not available", http.StatusNotFound)
		}
		return
	}
	if name == "manifest.json" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeFileFS(w, r, os.DirFS(m.cfg.FirmwareDir), name)
}

// --- the API for people ------------------------------------------------------------------------

type appView struct {
	ID       string `json:"id"` // "up:<id>" for the linked sm.w42.eu's apps
	Name     string `json:"name"`
	URL      string `json:"url,omitempty"`
	Web      string `json:"web"`
	Upstream bool   `json:"upstream,omitempty"`
}

type robotView struct {
	ID         string    `json:"id"`
	Created    time.Time `json:"created"`
	Public     bool      `json:"public"`
	Start      string    `json:"start,omitempty"`
	Apps       []string  `json:"apps"`
	RemoteApps bool      `json:"remote_apps"` // its apps can be changed here (managed.go)
	AskPin     bool      `json:"ask_pin"`
	Waiting    []string  `json:"waiting,omitempty"` // apps added here the robot has not got yet

	Name     string     `json:"name,omitempty"`
	Firmware string     `json:"firmware,omitempty"`
	Version  int32      `json:"version"` // of its app list here
	Applied  int32      `json:"applied"` // the version the robot has
	Online   bool       `json:"online"`  // its channel is open now
	LastSeen *time.Time `json:"last_seen,omitempty"`
	LastApp  string     `json:"last_app,omitempty"` // the page's id of the app it is on
	AppName  string     `json:"app_name,omitempty"` // its name on the robot
	Conn     string     `json:"conn,omitempty"`
	Question *Question  `json:"question,omitempty"`
	Answer   string     `json:"answer,omitempty"`
	Asked    string     `json:"asked,omitempty"`
	Stuck    bool       `json:"stuck,omitempty"`
	Privacy  string     `json:"privacy,omitempty"`        // the camera and mic, as set on the robot
	CamOff   string     `json:"camera_mic_off,omitempty"` // why they are off now
	History  []Event    `json:"history"`
	// Browsers paired with it, by app (pairings.go).
	Pairings []pairingView `json:"pairings"`
	// Managed by sm.w42.eu now (S16): this home manager is no longer its primary.
	Moved string `json:"moved,omitempty"`
	// Set up with another manager over USB (S19): it talks only to that one now.
	Left *Left `json:"left,omitempty"`
	// The manager is off on the robot ("robot" or "manager": who turned it off); "pending": turned
	// off on a page, the robot gets it when it connects.
	Off string `json:"off,omitempty"`
}

// robotViewLocked is a robot on the owner's page. m.mu held.
func (m *Manager) robotViewLocked(rb *Robot) robotView {
	v := robotView{ID: rb.ID, Created: rb.Created, Public: rb.Public, Start: rb.Start, Apps: robotAppIDs(rb),
		RemoteApps: rb.RemoteApps && m.signer != nil, AskPin: rb.AskPin,
		Name: rb.Name, Firmware: rb.Firmware, Version: rb.Version, Applied: rb.State.AppsVersion,
		Online: m.conns[rb.ID] != nil, AppName: rb.State.AppName, Answer: rb.State.Answer, Asked: rb.Asked,
		History: rb.History, Pairings: m.pairingViews(rb), Moved: rb.Moved, Left: rb.Left, Off: rb.Off,
		Privacy: rb.State.Privacy}
	if rb.DisablePending && rb.Off == "" {
		v.Off = "pending"
	}
	if v.Online {
		v.Conn, v.Question, v.Stuck, v.CamOff = rb.State.Conn, rb.State.Question, rb.State.Stuck, rb.State.CameraMicOff
	}
	for _, id := range v.Apps {
		if u := m.appURLLocked(rb, id); u != "" && appID(u) == rb.State.App {
			v.LastApp = id
		}
	}
	if !rb.LastSeen.IsZero() {
		t := rb.LastSeen
		v.LastSeen = &t
	}
	if v.History == nil {
		v.History = []Event{}
	}
	for id := range rb.Pending {
		v.Waiting = append(v.Waiting, id)
	}
	sort.Strings(v.Waiting)
	return v
}

// GET /api/me: whether this browser may act here, the catalog, the robots and the link.
func (m *Manager) handleMe(w http.ResponseWriter, r *http.Request) {
	apps := []appView{}
	for _, a := range m.apps {
		apps = append(apps, appView{ID: a.ID, Name: a.Name, URL: a.URL, Web: a.Web})
	}
	m.mu.Lock()
	for _, a := range m.linkSt.Catalog { // the linked sm.w42.eu's apps
		apps = append(apps, appView{ID: upstreamKey(a.ID), Name: a.Name, Web: a.Web, Upstream: true})
	}
	m.mu.Unlock()
	me := map[string]any{"apps": apps, "manager": m.managerName()}
	if !m.owner(w, r) {
		me["signed_in"] = false
		writeJSON(w, http.StatusOK, me)
		return
	}
	robots := []robotView{}
	m.mu.Lock()
	for _, rb := range m.robots {
		robots = append(robots, m.robotViewLocked(rb))
	}
	if m.cfg.UpstreamURL != "" {
		me["link"] = m.linkViewLocked()
	}
	m.mu.Unlock()
	sort.Slice(robots, func(i, j int) bool { return robots[i].ID < robots[j].ID })
	me["signed_in"] = true
	me["phone"] = !isLocal(r)
	me["robots"] = robots
	me["remote_apps"] = m.signer != nil // apps can be managed from here (if the robot allows it)
	if wifi := currentWifi(); wifi.SSID != "" {
		me["wifi"] = wifi // the page on this computer may fill in the Wi-Fi it is on
	}
	writeJSON(w, http.StatusOK, me)
}

// POST /api/my/robots/{id}/setup {"apps": ["raw", "pet"], "start": "pet"}: add the robot (or
// update one of yours), approve these apps, and get what the setup page writes into the robot:
// each app with a new token (the old ones stop working) and the one it starts with.
func (m *Manager) handleRobotSetup(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if !m.owner(w, r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	var req struct {
		Apps       []string `json:"apps"`
		Start      string   `json:"start"`
		RemoteApps *bool    `json:"remote_apps"` // may the manager change the apps later (default yes)
		AskPin     *bool    `json:"ask_pin"`     // does the robot ask before a new start app (default yes)
		Firmware   string   `json:"firmware"`    // what the robot reported over USB (hello), for the owner's page
		Installed  bool     `json:"installed"`   // the setup installed that firmware (else it kept the robot's)
		Second     bool     `json:"second"`      // the linked sm.w42.eu may become its primary on its screen (S16)
	}
	if !robotIDPattern.MatchString(id) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil || len(req.Apps) == 0 {
		http.Error(w, `body must be {"apps": ["<app id>", …], "start": "<app id>"}`, http.StatusBadRequest)
		return
	}
	if req.Start == "" {
		req.Start = req.Apps[0]
	}
	if !slices.Contains(req.Apps, req.Start) {
		http.Error(w, "the start app must be one of the apps", http.StatusBadRequest)
		return
	}
	type server struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		URL   string `json:"url"`
		Token string `json:"token"`
		E2E   bool   `json:"e2e,omitempty"`
	}
	servers := []server{}
	grants := map[string]appGrant{}
	now := time.Now()
	for _, appID := range req.Apps {
		if up, ok := strings.CutPrefix(appID, "up:"); ok { // the linked sm.w42.eu's: granted before
			m.mu.Lock()
			var ua *UpApp
			if rb := m.robots[id]; rb != nil {
				ua = rb.Upstream[up]
			}
			m.mu.Unlock()
			if ua == nil {
				http.Error(w, "add "+appID+" online first (it needs a token from the linked manager)", http.StatusBadRequest)
				return
			}
			servers = append(servers, server{appID, ua.Name, ua.URL, ua.Token, ua.E2E})
			continue
		}
		app := m.app(appID)
		if app == nil {
			http.Error(w, "no app "+appID, http.StatusBadRequest)
			return
		}
		if _, dup := grants[appID]; dup {
			continue
		}
		token, hash := app.token, ""
		if app.TokenFile == "" && !app.RobotToken {
			token = randHex(32)
			sum := sha256.Sum256([]byte(token))
			hash = hex.EncodeToString(sum[:])
		}
		grants[appID] = appGrant{Hash: hash, Created: now}
		servers = append(servers, server{app.ID, app.Name, app.URL, token, app.E2E})
	}
	m.mu.Lock()
	rb := m.robots[id]
	if rb == nil {
		rb = &Robot{ID: id, Owner: localOwner.Key, OwnerName: localOwner.Name, Created: now}
		m.robots[id] = rb
	}
	rb.Apps, rb.Start, rb.Pending = grants, req.Start, nil
	var keepUp []string // the linked sm.w42.eu's apps: those in this setup stay, the others go there too
	for _, id := range req.Apps {
		if up, ok := strings.CutPrefix(id, "up:"); ok {
			keepUp = append(keepUp, up)
		}
	}
	m.setUpstreamAppsLocked(rb, keepUp)
	rb.RemoteApps = req.RemoteApps == nil || *req.RemoteApps
	rb.AskPin = req.AskPin == nil || *req.AskPin
	rb.Version++
	rb.State.AppsVersion = rb.Version                                 // written over the cable just now
	rb.Moved, rb.Left, rb.Off, rb.DisablePending = "", nil, "", false // a USB setup turns the manager on
	channelToken := randHex(32)
	sum := sha256.Sum256([]byte(channelToken))
	rb.ChannelHash = hex.EncodeToString(sum[:])
	if fw := cleanName(req.Firmware); fw != "" {
		rb.Firmware = fw
	}
	what := "apps written: " + strings.Join(req.Apps, ", ") + ", starts with " + req.Start
	if req.Installed {
		what = "set up with " + rb.Firmware + "; " + what
	}
	if m.signer != nil {
		what += map[bool]string{true: "; changes from here allowed", false: "; changes only over USB"}[rb.RemoteApps]
	}
	m.noteLocked(rb, "usb", what)
	version, seq := rb.Version, rb.Seq
	start := m.appURLLocked(rb, req.Start)
	if start == "" {
		for _, sv := range servers {
			if sv.ID == req.Start {
				start = sv.URL
			}
		}
	}
	m.mu.Unlock()
	m.requestSave()
	m.log.Info("robot set up", "robot", id, "apps", req.Apps, "start", req.Start)
	res := map[string]any{"servers": servers, "start": start}
	if key := m.publicKey(); key != "" { // the robot's primary: its channel, its key for what it signs
		res["manager"] = map[string]any{"key": key, "name": m.managerName(), "version": version, "seq": seq,
			"url": m.robotURL(), "token": channelToken, "page": m.pageURL(),
			"remote_apps": rb.RemoteApps, "ask_pin": rb.AskPin}
	}
	if req.Second { // the linked sm.w42.eu as the second manager (S16)
		second, err := m.linkStandby(r.Context(), id)
		if err != nil {
			http.Error(w, "the linked manager: "+err.Error(), http.StatusBadGateway)
			return
		}
		res["manager2"] = second
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /api/my/robots/{id}/access {"public": bool}: whether the robot's code pairs anyone.
func (m *Manager) handleRobotAccess(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	var req struct {
		Public *bool `json:"public"`
	}
	if !m.owner(w, r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req) != nil || req.Public == nil {
		http.Error(w, `sign in, then {"public": true|false}`, http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	rb := m.robots[r.PathValue("id")]
	if rb == nil {
		m.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if rb.Public != *req.Public {
		rb.Public = *req.Public
		m.noteLocked(rb, "online", map[bool]string{true: "made public", false: "made private"}[rb.Public])
	}
	m.mu.Unlock()
	m.requestSave()
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/my/robots/{id}: remove a robot; all its app tokens stop working.
func (m *Manager) handleRemoveRobot(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	ok := m.owner(w, r)
	m.mu.Lock()
	rb := m.robots[r.PathValue("id")]
	if !ok || rb == nil {
		m.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	delete(m.robots, rb.ID)
	m.mu.Unlock()
	m.requestSave()
	m.log.Info("robot removed", "robot", rb.ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- the API for apps --------------------------------------------------------------------------

// RobotAuth is the answer to an app's POST /api/robot-auth.
type RobotAuth struct {
	OK        bool     `json:"ok"`
	Owner     string   `json:"owner,omitempty"`      // Robot.Owner
	OwnerName string   `json:"owner_name,omitempty"` // for display
	Public    bool     `json:"public,omitempty"`
	CacheS    int      `json:"cache_s,omitempty"` // the app may keep the answer this long
	Unpair    []string `json:"unpair,omitempty"`  // pairings the owner removed here (pairings.go)
}

// POST /api/robot-auth {"robot", "token"} with "Authorization: Bearer <the app's secret>": is
// this the robot's token for this app, and whose robot is it.
func (m *Manager) handleRobotAuth(w http.ResponseWriter, r *http.Request) {
	app := m.appBySecret(r)
	if app == nil {
		http.Error(w, "unknown app", http.StatusUnauthorized)
		return
	}
	var req struct {
		Robot string `json:"robot"`
		Token string `json:"token"`
		Seen  *struct {
			Pairings []Pairing `json:"pairings"` // nil: the app did not say
		} `json:"seen"` // the robot is connected to this app now: who is paired with it
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req) != nil {
		http.Error(w, `body must be {"robot", "token"}`, http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	rb := m.robots[req.Robot]
	var g appGrant
	var res RobotAuth
	if rb != nil {
		g = rb.Apps[app.ID]
		res = RobotAuth{Owner: rb.Owner, OwnerName: rb.OwnerName, Public: rb.Public}
	}
	m.mu.Unlock()
	want, err := hex.DecodeString(g.Hash)
	if err != nil || len(want) != sha256.Size {
		want = make([]byte, sha256.Size) // compare anyway: timing must not tell which robots exist
	}
	sum := sha256.Sum256([]byte(req.Token))
	if subtle.ConstantTimeCompare(sum[:], want) != 1 || rb == nil || req.Token == "" {
		writeJSON(w, http.StatusOK, RobotAuth{})
		return
	}
	res.OK, res.CacheS = true, int(authCacheHint/time.Second)
	m.mu.Lock()
	if rb.Pending[app.ID] == req.Token { // the robot has this app's new token: no longer kept here
		delete(rb.Pending, app.ID)
		m.requestSave()
	}
	if req.Seen != nil && req.Seen.Pairings != nil {
		m.pairingsSeenLocked(rb, app.ID, req.Seen.Pairings)
	}
	res.Unpair = rb.Unpair[app.ID]
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, res)
}

// appBySecret is the app that authenticated with "Authorization: Bearer <its secret>".
func (m *Manager) appBySecret(r *http.Request) *App {
	secret, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if secret == "" {
		return nil
	}
	got := sha256.Sum256([]byte(secret))
	var app *App
	for _, a := range m.apps {
		if a.SecretFile != "" && subtle.ConstantTimeCompare(got[:], a.secretHash[:]) == 1 {
			app = a
		}
	}
	return app
}

// --- state -------------------------------------------------------------------------------------

type stateFile struct {
	Robots map[string]*Robot    `json:"robots"`
	Link   *linkState           `json:"link,omitempty"`   // this home's link (link.go)
	Phones map[string]time.Time `json:"phones,omitempty"` // phones signed in at a robot (phone.go)
}

func (m *Manager) loadState() error {
	if m.cfg.State == nil {
		return nil
	}
	b, err := m.cfg.State.Load(context.Background())
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("%s: %w", m.cfg.State.Where(), err)
	}
	if st.Robots != nil {
		m.robots = st.Robots
	}
	if st.Link != nil {
		m.linkSt = *st.Link
	}
	if st.Phones != nil {
		m.phones = st.Phones
	}
	m.lastSaved = b
	m.log.Info("state loaded", "state", m.cfg.State.Where(), "robots", len(m.robots), "phones", len(m.phones))
	return nil
}

// SaveState writes the state if it changed, atomically, readable only by this user (session ids
// are credentials).
func (m *Manager) SaveState() error {
	if m.cfg.State == nil {
		return nil
	}
	m.mu.Lock()
	var link *linkState
	if m.linkSt.Token != "" || m.linkSt.Pending != "" {
		l := m.linkSt
		link = &l
	}
	b, err := json.MarshalIndent(stateFile{Robots: m.robots, Link: link, Phones: m.phones}, "", "  ")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if bytes.Equal(b, m.lastSaved) {
		return nil
	}
	if err := m.cfg.State.Save(context.Background(), b); err != nil {
		return err
	}
	m.lastSaved = b
	return nil
}

// RunStateSaver saves periodically and soon after changes until ctx ends.
func (m *Manager) RunStateSaver(ctx context.Context) {
	if m.cfg.State == nil {
		return
	}
	t := time.NewTicker(stateSaveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.saveNow:
		}
		if err := m.SaveState(); err != nil {
			m.log.Warn("state not saved", "state", m.cfg.State.Where(), "err", err)
		}
	}
}

func (m *Manager) requestSave() {
	select {
	case m.saveNow <- struct{}{}:
	default:
	}
}

// --- small helpers -----------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
