# mpfpv2

UDP relay mesh for FPV drones. Single binary, userspace TUN, multipath redundant transport.

All traffic goes through a central server (star topology). No encryption — `teamKey` is for pairing only, not security.

## Architecture

```
Drone (client)                    Server                     Ground Station (client)
  App → TUN → +8B hdr → UDP ──→  recv → dedup → route ──→  UDP → -8B hdr → TUN → App
              (all NICs)                         (all addrs)
```

- **Redundant mode only**: every packet sent through all available NICs
- **8-byte header**: Version(4bit) + Type(4bit) + Reserved(1B) + ClientID(2B) + Seq(4B)
- **Fixed MTU**: 1400 (36 bytes overhead per packet, 97.4% payload efficiency)

## Quick Start

### Server

```bash
wget http://YOUR_SERVER:9801/download/mpfpv-linux-amd64 -O mpfpv-server
chmod +x mpfpv-server

cat > mpfpv.yml << EOF
mode: server
teamKey: "your-team-key"
server:
  listenAddr: "0.0.0.0:9800"
  virtualIP: "10.99.0.254/24"
  subnet: "10.99.0.0/24"
  webUI: "0.0.0.0:9801"
EOF

sudo ./mpfpv-server -config mpfpv.yml
```

Web UI: `http://YOUR_SERVER:9801/`

### Client (zero-config)

```bash
# Linux
wget http://YOUR_SERVER:9801/download/mpfpv-linux-amd64 -O mpfpv
chmod +x mpfpv
sudo ./mpfpv 'mpfpv://your-team-key@YOUR_SERVER:9800'

# Or with config file
sudo ./mpfpv -config mpfpv.yml
```

That's it. The client auto-generates a persistent ID, discovers NICs, and connects.

### Windows

Download `mpfpv-windows.exe` from the server Web UI or `http://YOUR_SERVER:9801/download/mpfpv-windows.exe`.

Place `mpfpv.yml` next to the exe:

```yaml
mode: client
teamKey: "your-team-key"
client:
  serverAddr: "YOUR_SERVER:9800"
```

Double-click to run (auto-elevates to admin for TUN).

### OpenIPC / Embedded ARM

```bash
wget http://YOUR_SERVER:9801/download/mpfpv-linux-arm32 -O /usr/bin/mpfpv
chmod +x /usr/bin/mpfpv
mpfpv 'mpfpv://your-team-key@YOUR_SERVER:9800'
```

## Connection String

Format: `mpfpv://TEAMKEY@HOST:PORT`

The server Web UI shows a copyable connection string. Clients can use it directly as a command-line argument — no config file needed.

## Server Configuration

```yaml
mode: server
teamKey: "your-team-key"        # Pairing key (not encryption)

server:
  listenAddr: "0.0.0.0:9800"    # UDP listen
  virtualIP: "10.99.0.254/24"   # Server's virtual IP
  subnet: "10.99.0.0/24"        # IP pool for clients
  clientTimeout: 30              # Session timeout (seconds)
  addrTimeout: 15                # Per-address timeout (seconds)
  dedupWindow: 4096              # Dedup sliding window size
  ipPoolFile: "ip_pool.json"     # IP allocation persistence
  webUI: "0.0.0.0:9801"         # Web UI + API (empty = disabled)
```

## Client Configuration

```yaml
mode: client
teamKey: "your-team-key"

client:
  serverAddr: "1.2.3.4:9800"    # Server address
  # bindInterface: "eth0"        # Optional: force single NIC
  # dedupWindow: 4096            # Optional: dedup window size
```

Most fields are optional — the client auto-discovers NICs and auto-generates its ID.

## API

The server exposes a JSON API for OSD integration (per-NIC RTT, bandwidth):

```bash
# All clients with per-path stats
curl http://SERVER:9801/api/clients

# Response:
# [{"clientID":43481,"virtualIP":"10.99.0.2","deviceName":"drone",
#   "online":true,"pathRTTs":[{"name":"usb0","rttMs":30,"txBytes":354114204}],
#   "rxBytes":751617,"txBytes":5267}]

# Other endpoints
GET /api/routes              # Route table
GET /api/stats               # Global stats (rx/tx bytes, device count)
GET /api/connection-string   # Connection string for clients
GET /api/server-config       # Server config (GET/POST)
DELETE /api/clients/{id}     # Kick a client

# Client downloads
GET /download/mpfpv-linux-amd64
GET /download/mpfpv-linux-arm64
GET /download/mpfpv-linux-arm32
GET /download/mpfpv-windows.exe
```

## Build

Requires Go 1.23+.

```bash
# Server
CGO_ENABLED=0 go build -ldflags "-s -w" -o mpfpv-server ./server/cmd/

# Client (Linux)
CGO_ENABLED=0 go build -ldflags "-s -w" -o mpfpv ./client/cmd/

# Client (Windows, includes embedded wintun.dll)
CGO_ENABLED=0 GOOS=windows go build -ldflags "-s -w" -o mpfpv.exe ./client/cmd-windows/

# Cross-compile
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "-s -w" -o mpfpv-arm32 ./client/cmd/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" -o mpfpv-arm64 ./client/cmd/
```

## Binary Sizes

| Binary | Size | UPX |
|--------|------|-----|
| Server (Linux amd64) | 6.7 MB | — |
| Client (Linux amd64) | 3.3 MB | — |
| Client (Linux ARM32) | 3.2 MB | 1.3 MB |
| Client (Windows) | 4.0 MB | — |

## Key Design Decisions

- **No encryption**: designed for FPV video relay where latency matters more than secrecy
- **Redundant only**: no failover mode — simpler, more reliable
- **Minimal buffering**: socket buffers and TUN queues kept small to avoid stale-packet delay
- **PathProbing**: new NICs are probed via heartbeat before carrying data traffic
- **5ms write deadline on server**: one slow client cannot stall forwarding for all others
- **Zero-config client**: connection string contains everything needed to connect

## systemd Service

```ini
# /etc/systemd/system/mpfpv.service
[Unit]
Description=mpfpv2 relay server
After=network.target

[Service]
Type=simple
ExecStart=/opt/mpfpv/mpfpv -config /opt/mpfpv/mpfpv.yml
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

## License

MIT
