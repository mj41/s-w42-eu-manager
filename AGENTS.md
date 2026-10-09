# s-w42-eu-manager

A home's own Stackchan manager (no sign-in: the computer it runs on is the owner, phones sign in at
a robot): robots, the app catalog, setup over USB, tokens per robot per app, `robot-auth` for the
apps, and the link up to an upstream manager (e.g. sm.w42.eu). See [README.md](README.md). Design: home-w42-eu
`docs/stackchan-sites.md`.

- The author's workspace notes (private, `../s-w42-eu-mj-priv/AGENTS.md`), when present, cover build, flash and running everything locally. Read them first.
- Go only: the standard library, gorilla/websocket, go.bug.st/serial (`s-w42-eu-usb`), and s-w42-eu-raw's `statestore` (Postgres through pgx).
- Apps (e.g. s-w42-eu-raw) call `POST /api/robot-auth`; change it together with their client (`internal/server/manager.go` in s-w42-eu-raw).
- The setup page talks to the robot with the USB setup protocol of the firmware (`usb_setup.h` in mj41/StackChan, branch `embody-mj41`).
- Before committing, run `gofmt -l .`, `go vet ./...` and `go test -race ./...`.
