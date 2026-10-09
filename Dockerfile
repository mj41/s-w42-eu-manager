# syntax=docker/dockerfile:1

FROM golang:1.26.8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/s-w42-eu-manager ./cmd/s-w42-eu-manager

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/s-w42-eu-manager /s-w42-eu-manager
EXPOSE 8790
USER nonroot:nonroot
# Pass the catalog (-apps-file, with the apps' secret files) and the addresses robots and phones
# reach it at (-robot-url, -page-url); mount a volume for -state-file (robots, the link, signed-in
# phones) and set XDG_CACHE_HOME to a writable place for the firmware cache (the root file system
# is read-only). The page's owner is the computer it runs on (loopback): use the host's network.
ENTRYPOINT ["/s-w42-eu-manager", "-listen", ":8790"]
