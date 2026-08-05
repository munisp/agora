# ---------- build ----------
FROM golang:1.23-alpine AS build
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server

# ---------- run ----------
FROM gcr.io/distroless/static-debian12:nonroot
# tzdata is required: the availability engine evaluates weekly rules in the
# tenant's IANA timezone (time.LoadLocation).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/server /server
EXPOSE 7002
USER nonroot:nonroot
ENTRYPOINT ["/server"]
