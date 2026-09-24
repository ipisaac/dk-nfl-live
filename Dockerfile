FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /app .

# Temporary: Node 22.20 base so probe.go can run probe.mjs.
FROM node:22.20-alpine
COPY --from=build /app /app
COPY probe.mjs /probe.mjs
USER node
EXPOSE 8080
ENTRYPOINT ["/app"]
