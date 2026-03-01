# homekit-rtsp-proxy

A Go service that bridges Apple HomeKit cameras to standard RTSP streams. It pairs with HomeKit-native cameras over the local network, decrypts their SRTP video/audio streams in real time, and re-serves them as plain RTSP. An ONVIF-compatible event server relays motion detection events. Designed for integration with [Scrypted](https://github.com/koush/scrypted) and [Home Assistant](https://www.home-assistant.io/).

## Features

- **Zero video transcoding** -- SRTP is decrypted to plain RTP; the H.264 bitstream passes through untouched
- **Audio transcoding** -- AAC-ELD (HomeKit-only codec) is transcoded to AAC-LC (universally supported) via libfdk-aac
- **On-demand streaming** -- the camera stream starts when the first RTSP client connects and stops when the last disconnects
- **ONVIF motion events** -- subscribes to HomeKit motion sensor characteristics and exposes them via ONVIF PullPoint, so Scrypted/Frigate can consume motion events
- **Multi-camera support** -- each camera gets its own RTSP port and ONVIF port with independent lifecycles
- **Custom HAP implementation** -- includes a from-scratch pair-verify handshake and encrypted session layer to work around upstream library bugs

## How It Works

```
HomeKit Camera                homekit-rtsp-proxy                 RTSP Clients
                                                                (Scrypted, ffmpeg, VLC)
      |                              |                              |
      |<--- mDNS discovery -------->|                              |
      |<--- HAP pair-setup -------->|                              |
      |<--- HAP pair-verify ------->|                              |
      |                              |                              |
      |                              |<---- RTSP DESCRIBE/PLAY ----|
      |<--- HAP StartStream --------|                              |
      |==== SRTP (encrypted) ======>|                              |
      |                              |---- RTP (decrypted) ------->|
      |                              |                              |
      |--- HAP motion event ------->|                              |
      |                              |---- ONVIF PullMessages ---->|
```

1. Discovers HomeKit cameras via mDNS and pairs using the setup code
2. When an RTSP client connects, negotiates an SRTP stream with the camera via HAP TLV8
3. Receives encrypted SRTP packets, decrypts them, and forwards plain RTP to the RTSP client
4. Sends RTCP keepalives and audio return silence packets to keep the camera streaming
5. Subscribes to the camera's motion sensor and relays events via an ONVIF PullPoint server

## Requirements

- Go 1.25+
- `libfdk-aac-dev` (build time)
- `libfdk-aac2` (runtime)
- A HomeKit-compatible camera on the same LAN
- The camera's 8-digit HomeKit setup code

## Building

### With Docker (recommended for ARM64/Raspberry Pi)

```bash
docker build -t homekit-rtsp-proxy .
```

### Natively

```bash
# Install libfdk-aac development headers (Debian/Ubuntu)
sudo apt-get install libfdk-aac-dev

go build -o homekit-rtsp-proxy ./cmd/homekit-rtsp-proxy/
```

### Cross-compile for Raspberry Pi

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -o homekit-rtsp-proxy ./cmd/homekit-rtsp-proxy/
```

Note: Cross-compiling with CGo requires an ARM64 cross-compilation toolchain. Building natively on the Pi (or via Docker) is simpler.

## Configuration

Copy the example config and edit it:

```bash
cp configs/config.example.yaml config.yaml
```

```yaml
log_level: info
pairing_store: ./pairings.json

cameras:
  - name: "Camera-E1-XXXX"           # Must match the camera's mDNS name
    setup_code: "031-45-154"          # HomeKit setup code
    rtsp:
      port: 8554
      path: "/live"
    video:
      width: 1920
      height: 1080
      fps: 30
      max_bitrate: 2000              # kbps
    audio:
      enabled: true
      codec: "aac-eld"               # "aac-eld" or "opus"
      sample_rate: 16000
      gain: 512                      # PCM gain for transcoding (0 = passthrough, default 512 ≈ 54dB)
    onvif:
      enabled: true
      port: 8580
```

### Audio gain

The `audio.gain` setting controls PCM amplification during AAC-ELD to AAC-LC transcoding. Some HomeKit cameras (notably Aqara Camera E1) produce very quiet AAC-ELD audio -- the decoded PCM peaks at ~7 out of 32767, roughly 54dB below normal levels. The gain multiplier compensates for this.

| Value | Effect |
|-------|--------|
| `512` | Default. Amplifies ~54dB to bring quiet cameras to normal levels |
| `0` | No amplification -- decoded PCM passes through unchanged |
| `1` - `1024` | Custom amplification. Higher values = louder. Output is hard-clipped at 16-bit range (±32767) |

Set `gain: 0` if your camera's audio is already at normal levels, or if you don't need audio amplification.

### Logging

All logs are written to **stdout** using Go's structured `slog` package. Control verbosity with `log_level` in the config file:

- `debug` -- verbose protocol details, RTP packet counts, HAP characteristic dumps
- `info` -- connection events, stream start/stop, motion events (default)
- `warn` -- recoverable errors, non-fatal stream issues
- `error` -- fatal failures

Use your process manager (systemd, Docker, etc.) to capture and rotate logs as needed.

### Finding your camera's name

The camera's mDNS name is advertised on the local network. You can find it by running the proxy with `log_level: debug` -- it will log all discovered devices during startup. Alternatively, check the Home app on your iPhone.

### Multiple cameras

Add additional entries under `cameras:` with different RTSP and ONVIF ports:

```yaml
cameras:
  - name: "Front Door"
    setup_code: "031-45-154"
    rtsp:
      port: 8554
      path: "/live"
    onvif:
      port: 8580

  - name: "Back Yard"
    setup_code: "123-45-678"
    rtsp:
      port: 8555
      path: "/live"
    onvif:
      port: 8581
```

## Running

```bash
./homekit-rtsp-proxy -config config.yaml
```

On first run, the proxy will pair with the camera using the setup code. Pairing keys are persisted to `.hkontroller/` and `pairings.json`, so subsequent runs reconnect automatically.

### Docker Compose

```yaml
services:
  homekit-rtsp-proxy:
    container_name: homekit-rtsp-proxy
    build: .
    restart: unless-stopped
    network_mode: host           # Required for mDNS discovery
    volumes:
      - ./data:/app/data:rw      # Persist config + pairing keys
    working_dir: /app/data
```

Place `config.yaml` in the `data/` directory. Host networking is required because mDNS uses multicast, and the camera sends SRTP to the host's real LAN IP.

### Verifying the stream

```bash
# Check stream info
ffprobe rtsp://localhost:8554/live

# Play the stream
ffplay rtsp://localhost:8554/live

# Record a clip
ffmpeg -i rtsp://localhost:8554/live -c copy -t 10 clip.mp4
```

## Scrypted Integration

1. In Scrypted, add an ONVIF camera plugin
2. Set the ONVIF address to `http://<proxy-host>:8580`
3. Scrypted will discover the camera profile and RTSP stream URL automatically
4. Motion events will flow through the ONVIF PullPoint subscription

## Architecture

```
cmd/homekit-rtsp-proxy/main.go       Entry point & orchestration
internal/config/config.go             YAML configuration
internal/hap/
  controller.go                       HAP controller (discovery, pairing, stream management)
  pair_verify.go                      Custom pair-verify handshake (Curve25519 + Ed25519)
  encrypted_conn.go                   ChaCha20-Poly1305 HAP session encryption
  hap_client.go                       Encrypted HTTP client for characteristic access
  camera.go                           CameraRTPStreamManagement TLV8 codec
  tlv8.go                             TLV8 encoder/decoder
  pairing_store.go                    JSON persistence for pairing keys
internal/stream/
  proxy.go                            SRTP decryption + RTCP keepalive + audio return
  rtsp_server.go                      gortsplib RTSP server + IDR caching
  session.go                          On-demand stream lifecycle state machine
  audio_transcoder.go                 CGo libfdk-aac AAC-ELD to AAC-LC transcoder
internal/onvif/
  server.go                           ONVIF HTTP/SOAP server
  pullpoint.go                        PullPoint subscription manager
  templates.go                        SOAP XML response templates
```

See [DESIGN.md](DESIGN.md) for the full design document and protocol specification.

## Debug Tools

### debug-pair

Standalone tool for testing HAP pairing independently:

```bash
go build -o debug-pair ./cmd/debug-pair/
./debug-pair "Camera-E1-XXXX" "031-45-154"
```

Performs mDNS discovery, pair-setup, pair-verify, and dumps the full accessory database.

### test-rtsp

Minimal RTSP server that sends synthetic H.264 frames for testing client compatibility:

```bash
go build -o test-rtsp ./cmd/test-rtsp/
./test-rtsp
# Serves on rtsp://localhost:8555/test
```

## Known Limitations

- **AAC-ELD only tested:** Opus codec negotiation is implemented but untested. Aqara Camera E1 rejects Opus.
- **Single stream per camera:** Uses one of the camera's limited concurrent stream slots.
- **No automatic stream recovery:** If the camera drops the SRTP stream, the RTSP client must reconnect.
- **Audio gain may need tuning:** The default gain of 512 (~54dB) compensates for quiet AAC-ELD output on some cameras. Set `gain: 0` to disable amplification.
- **No talkback:** Silence packets satisfy the protocol, but real two-way audio is not implemented.

## Tested Cameras

| Camera | Video | Audio | Motion Events |
|--------|-------|-------|---------------|
| Aqara Camera E1 | 1080p30 H.264 | AAC-ELD 16kHz | Yes (HAP events) |

## License

MIT
