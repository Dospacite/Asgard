FROM node:25-alpine AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN npm install -g npm@11.6.2 && npm ci
COPY web/ ./
COPY internal/frontend/dist/ /src/internal/frontend/dist/
RUN npm run build

FROM golang:1.26-alpine AS go
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/frontend/dist/ internal/frontend/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=0.2.0" -o /out/asgard ./cmd/asgard

FROM alpine:3.23
# openssh-client is what makes an `ssh` Git credential usable: clones over
# git@ build a GIT_SSH_COMMAND around the stored key, and without the binary
# every SSH-backed import and re-sync fails with "ssh: not found".
RUN apk add --no-cache ca-certificates tzdata git tar openssh-client && addgroup -S asgard && adduser -S -G asgard asgard
COPY --from=go /out/asgard /usr/local/bin/asgard
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=20s --timeout=3s --start-period=10s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/readyz || exit 1
ENTRYPOINT ["/usr/local/bin/asgard"]
CMD ["serve"]
