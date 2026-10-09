// Command s-w42-eu-manager is a home's own Stackchan manager: on its page people add their
// robots, approve the apps each robot may use (the catalog, -apps-file) and set robots up over
// USB with the official firmware and one token per approved app. Apps check robot tokens with it.
//
// Only the page on the computer it runs on may use it (loopback): that computer is the owner.
// Phones sign in by scanning the code on a robot's Manager screen.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mj41/s-w42-eu-manager/internal/manager"

	"github.com/mj41/s-w42-eu-raw/statestore"
)

func main() {
	var (
		listen      = flag.String("listen", ":8790", "HTTP listen address")
		publicURL   = flag.String("public-url", "", "the URL browsers use (default http://localhost:<port>, robots and phones get this computer's LAN address)")
		stateFile   = flag.String("state-file", defaultStateFile(), "JSON file with the robots, the link and signed-in phones (\"\" keeps them in memory only)")
		stateDB     = flag.String("state-database", os.Getenv("STATE_DATABASE_URL"), "keep the state in Postgres instead (postgres://user@host/db; the password from PGPASSWORD); a -state-file that exists is imported once, into an empty database")
		createKey   = flag.Bool("create-signing-key", true, "make a new signing key when -signing-key-file is missing (false when the key is provided, e.g. a mounted secret: a new one would cut every robot off)")
		uiDir       = flag.String("ui-dir", "", "development: serve the pages from this directory (e.g. internal/manager/ui)")
		appsFile    = flag.String("apps-file", defaultConfigFile("apps.json"), "the app catalog: a JSON list of {id, name, url, secret_file | token_file}")
		tlsListen   = flag.String("tls-listen", "", "also serve over HTTPS on this address, e.g. :8791 (Chrome's Web Serial needs a secure page: HTTPS, or localhost)")
		tlsCert     = flag.String("tls-cert", defaultConfigFile("tls-cert.pem"), "TLS certificate for -tls-listen; a self-signed one is created if missing")
		tlsKey      = flag.String("tls-key", defaultConfigFile("tls-key.pem"), "TLS key for -tls-listen")
		firmwareDir = flag.String("firmware-dir", "", "the official Embody Mode firmware (manifest.json and its parts) from this directory instead of GitHub")
		signingKey  = flag.String("signing-key-file", defaultConfigFile("signing-key.pem"), "P-256 key that signs robots' app lists, so their apps can be changed here without USB; created if missing (\"\" = only over USB)")
		firmwareRel = flag.String("firmware-release", "latest", "else from this GitHub release of mj41/StackChan: latest, a tag (embody-v…), or \"\" for none")
		name        = flag.String("name", "", "this manager's name for people and robots (default \"home on <this computer>\")")
		robotURL    = flag.String("robot-url", "", "where robots open their manager channel (default: ws://<this computer's LAN address>/robot, or ws(s)://<public host>/robot with -public-url)")
		pageURL     = flag.String("page-url", "", "this page's address shown on the robot's Manager screen (default: this computer's LAN address, or the public URL with -public-url)")
		upstreamURL = flag.String("upstream-url", "", "the manager to link up to, e.g. https://sm.w42.eu (\"\": no link)")
		debug       = flag.Bool("debug", false, "debug logging")
	)
	flag.Parse()
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *publicURL == "" {
		_, port, _ := net.SplitHostPort(*listen)
		*publicURL = "http://localhost:" + port
		// Robots and phones reach it on the LAN, at this computer's address.
		if ip := lanIP(); ip != "" {
			lan := net.JoinHostPort(ip, port)
			if *robotURL == "" {
				*robotURL = "ws://" + lan + "/robot"
			}
			if *pageURL == "" {
				*pageURL = "http://" + lan
			}
		}
	}
	var state statestore.Store
	if *stateDB != "" {
		openCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute) // a database that is down for a moment (retried), or another copy's lock
		var from *statestore.File
		if *stateFile != "" {
			from = &statestore.File{Path: *stateFile}
		}
		st, err := statestore.Open(openCtx, *stateDB, "sm", from)
		cancel()
		if err != nil {
			log.Error("state database", "err", err)
			os.Exit(1)
		}
		state = st
		log.Info("state in the database", "where", st.Where())
	}
	m, err := manager.New(manager.Config{
		PublicURL:        strings.TrimRight(*publicURL, "/"),
		StateFile:        *stateFile,
		State:            state,
		UIDir:            *uiDir,
		AppsFile:         existing(*appsFile),
		FirmwareDir:      *firmwareDir,
		FirmwareRelease:  *firmwareRel,
		FirmwareCacheDir: firmwareCache(),
		SigningKeyFile:   *signingKey,
		NoNewSigningKey:  !*createKey,
		Name:             *name,
		RobotURL:         *robotURL,
		PageURL:          *pageURL,
		UpstreamURL:      strings.TrimRight(*upstreamURL, "/"),
		Log:              log,
	})
	if err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}
	httpSrv := &http.Server{Addr: *listen, Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second}
	var httpsSrv *http.Server
	if *tlsListen != "" {
		cert, err := loadOrCreateCert(*tlsCert, *tlsKey, log)
		if err != nil {
			log.Error("TLS certificate", "err", err)
			os.Exit(1)
		}
		httpsSrv = &http.Server{Addr: *tlsListen, Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
		go func() {
			log.Info("serving over HTTPS", "listen", *tlsListen)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("https server", "err", err)
				os.Exit(1)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go m.RunStateSaver(ctx)
	if state != nil {
		go func() { // the database's lock is gone: another copy may write now; stop here
			select {
			case <-state.Lost():
				log.Error("state database: connection (and lock) lost; exiting")
				os.Exit(1)
			case <-ctx.Done():
			}
		}()
	}
	go m.RunLink(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if httpsSrv != nil {
			httpsSrv.Shutdown(shutdownCtx)
		}
		httpSrv.Shutdown(shutdownCtx)
	}()
	log.Info("s-w42-eu-manager listening", "listen", *listen, "public_url", *publicURL, "robot_url", *robotURL, "apps_file", *appsFile, "upstream", *upstreamURL)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
	if err := m.SaveState(); err != nil {
		log.Warn("state not saved", "file", *stateFile, "err", err)
	}
	if state != nil {
		state.Close() // releases the lock for the next copy
	}
}

// existing is path if the file exists, else "" (no catalog yet: the pages say so).
func existing(path string) string {
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func defaultStateFile() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "s-w42-eu-manager", "state.json")
}

func defaultConfigFile(name string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return name
	}
	return filepath.Join(dir, "s-w42-eu-manager", name)
}

func firmwareCache() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "s-w42-eu-manager", "firmware")
}

// lanIP is this computer's address on its network: the source address of its default route
// (nothing is sent).
func lanIP() string {
	c, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok && !a.IP.IsLoopback() {
		return a.IP.String()
	}
	return ""
}
