# Build the server. Two stages, so what ships is a static binary and nothing
# else - no toolchain, no module cache, no shell scripts that could drift from
# the flags cmd/server actually defines.
# A patch version, not a major line: govulncheck reports standard library
# advisories against the toolchain that built the binary, and the fix for those
# is to build with a newer one. Bump this when it reports something.
FROM golang:1.26.4-alpine AS build
WORKDIR /src

# Dependencies first, so editing Go files does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
# CGO off: pgx and coder/websocket are pure Go, and a static binary runs on
# scratch.
RUN CGO_ENABLED=0 go build -trimpath -o /ycollab-server ./cmd/server

FROM alpine:3.21
# wget is here for the healthcheck and nothing else; the image would otherwise
# have no way to report readiness to compose.
RUN apk add --no-cache wget ca-certificates
COPY --from=build /ycollab-server /usr/local/bin/ycollab-server
EXPOSE 8080
# Not root: the process needs no filesystem of its own.
USER nobody
ENTRYPOINT ["/usr/local/bin/ycollab-server"]
