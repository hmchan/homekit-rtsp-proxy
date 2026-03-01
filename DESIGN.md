# HomeKit Camera RTSP Proxy — Design Document & Specification

## 1. Overview

**homekit-rtsp-proxy** is a Go service that bridges Apple HomeKit cameras to standard RTSP streams. It acts as a HomeKit Accessory Protocol (HAP) controller, pairs with HomeKit-native cameras over the local network, negotiates encrypted SRTP video/audio streams, decrypts them in real time, and re-serves them as plain RTSP — all with zero transcoding of video. An ONVIF-compatible event server relays motion detection events. The primary integration target is Scrypted + Home Assistant.

### 1.1 Problem Statement

HomeKit cameras expose streams only through the proprietary HAP/SRTP protocol. Third-party home automation systems (Home Assistant, Scrypted, Frigate) require RTSP or ONVIF. No existing open-source tool bridges these two ecosystems with on-demand streaming and motion event relay.

### 1.2 Design Goals

| Goal | Approach |
|------|----------|
| Zero video transcoding | Decrypt SRTP → RTP passthrough (H.264 bitstream untouched) |
| On-demand streaming | Stream starts on first RTSP client, stops on last disconnect |
| Audio compatibility | Transcode AAC-ELD (HomeKit-only) → AAC-LC (universal) via libfdk-aac |
| Motion event relay | HAP characteristic subscription → ONVIF PullPoint events |
| Reliable deployment | Docker container with auto-restart, host networking for mDNS |
| Multi-camera support | Per-camera RTSP ports and ONVIF ports, independent lifecycles |

### 1.3 Non-Goals

- Recording, storage, or playback
- Camera PTZ control
- Two-way audio (talkback) — silence packets are sent to satisfy the protocol, but real audio return is not implemented
- Camera firmware updates or HomeKit accessory management
- Cloud connectivity

---

## 2. Architecture

### 2.1 System Context

```
┌──────────────────────────────────────────────────────────────────────┐
│                            Local Network                             │
│                                                                      │
│  ┌───────────────────┐    mDNS     ┌───────────────────────────┐    │
│  │   HomeKit Camera   │◄──────────►│   homekit-rtsp-proxy      │    │
│  │  (Aqara Camera E1) │            │                           │    │
│  │                    │   HAP/TCP  │  ┌─────────────────────┐ │    │
│  │                    │◄──────────►│  │  HAP Controller     │ │    │
│  │                    │            │  │  (pair, verify,     │ │    │
│  │                    │  SRTP/UDP  │  │   stream mgmt)      │ │    │
│  │                    │───────────►│  └─────────────────────┘ │    │
│  └───────────────────┘            │  ┌─────────────────────┐ │    │
│                                    │  │  SRTP Proxy         │ │    │
│                                    │  │  (decrypt → RTP)    │ │    │
│                                    │  └─────────────────────┘ │    │
│                                    │  ┌─────────────────────┐ │    │
│  ┌──────────────┐       RTSP/TCP  │  │  RTSP Server        │ │    │
│  │  Scrypted /   │◄──────────────►│  │  (gortsplib)        │ │    │
│  │  ffmpeg /     │                │  └─────────────────────┘ │    │
│  │  VLC          │                │  ┌─────────────────────┐ │    │
│  └──────────────┘                │  │  Audio Transcoder   │ │    │
│                                    │  │  (AAC-ELD → AAC-LC) │ │    │
│  ┌──────────────┐      ONVIF/HTTP │  └─────────────────────┘ │    │
│  │  Scrypted     │◄──────────────►│  ┌─────────────────────┐ │    │
│  │  (motion      │                │  │  ONVIF Server       │ │    │
│  │   events)     │                │  │  (PullPoint events) │ │    │
│  └──────────────┘                │  └─────────────────────┘ │    │
│                                    └───────────────────────────┘    │
└──────────────────────────────────────────────────────────────────────┘
```

### 2.2 Component Diagram

```
cmd/homekit-rtsp-proxy/main.go          ← Entry point, orchestration
    │
    ├── internal/config/                 ← YAML configuration
    │       config.go
    │
    ├── internal/hap/                    ← HomeKit Accessory Protocol
    │       controller.go                   HAP controller (discovery, pairing, streams)
    │       pair_verify.go                  Custom pair-verify (4-message handshake)
    │       encrypted_conn.go               ChaCha20-Poly1305 session encryption
    │       hap_client.go                   Encrypted HTTP (GET/PUT characteristics)
    │       camera.go                       CameraRTPStreamManagement TLV8 codec
    │       tlv8.go                         TLV8 encoder/decoder
    │       pairing_store.go                JSON persistence for pairing keys
    │
    ├── internal/stream/                 ← Media pipeline
    │       proxy.go                        SRTP → RTP decryption + RTCP keepalive
    │       rtsp_server.go                  gortsplib RTSP server + IDR caching
    │       session.go                      On-demand stream lifecycle state machine
    │       audio_transcoder.go             CGo libfdk-aac AAC-ELD → AAC-LC
    │
    └── internal/onvif/                  ← ONVIF compatibility
            server.go                       HTTP/SOAP endpoint router
            pullpoint.go                    PullPoint subscription manager
            templates.go                    SOAP XML response templates
```

### 2.3 Data Flow

```
Camera ──SRTP/UDP──► SRTPProxy.readVideoLoop()
                         │
                         ├─ SRTP? → DecryptRTP → parse rtp.Packet
                         │              │
                         │              └─► onVideoRTP callback
                         │                      │
                         │                      └─► RTSPServer.WriteVideoPacket()
                         │                              │
                         │                              ├─ Normalize seq/timestamp
                         │                              ├─ Cache IDR frames
                         │                              └─ stream.WritePacketRTP()
                         │                                      │
                         │                                      └─► RTSP clients
                         │
                         └─ SRTCP? → handleVideoRTCP → send ReceiverReport

Camera ──SRTP/UDP──► SRTPProxy.readAudioLoop()
                         │
                         └─ DecryptRTP → parse rtp.Packet
                                │
                                └─► onAudioRTP callback
                                        │
                                        └─► RTSPServer.WriteAudioPacket()
                                                │
                                                ├─ Strip AU header
                                                ├─ AudioTranscoder.Transcode()
                                                │      │
                                                │      ├─ Decode AAC-ELD → PCM
                                                │      ├─ Amplify ×gain (configurable, default 512)
                                                │      └─ Encode PCM → AAC-LC
                                                │
                                                ├─ Build new AU header
                                                └─ stream.WritePacketRTP()
```

---

## 3. Protocol Specifications

### 3.1 HomeKit Accessory Protocol (HAP)

The proxy implements a subset of the HAP specification (R15) as a **controller** (not an accessory). All HAP communication occurs over TCP with HTTP-like framing.

#### 3.1.1 Discovery (mDNS/DNS-SD)

- Service type: `_hap._tcp.local.`
- The proxy uses the `hkontroller` library for mDNS discovery
- Discovered devices expose their name, pairing state, and feature flags via TXT records
- Device names may contain backslash-escaped characters from mDNS (e.g., `Camera\\ E1`)

#### 3.1.2 Pair-Setup

Pair-setup is handled by the `hkontroller` library using SRP-6a (Secure Remote Password) with the camera's 8-digit setup code. This establishes long-term Ed25519 identity keys for both controller and accessory, persisted to disk.

**Stored artifacts:**
- `.hkontroller/keypair` — Controller's Ed25519 key pair (JSON: `{Public, Private}`)
- `.hkontroller/<deviceID>.pairing` — Accessory's public key and pairing metadata
- `pairings.json` — Additional pairing data (IP, port, keys) for reconnection

#### 3.1.3 Pair-Verify (Custom Implementation)

The proxy implements a custom pair-verify handshake because the `hkontroller` library incorrectly includes a `Method` TLV in M1, which some cameras reject.

**4-Message Handshake:**

```
Controller                              Accessory
    │                                       │
    │  M1: State=1, PublicKey(eph)          │
    │  (NO Method TLV)                      │
    ├──────────────────────────────────────►│
    │                                       │
    │  M2: State=2, PublicKey(eph),         │
    │      EncryptedData{ID, Signature}     │
    │◄──────────────────────────────────────┤
    │                                       │
    │  M3: State=3, EncryptedData{ID, Sig}  │
    ├──────────────────────────────────────►│
    │                                       │
    │  M4: State=4 (success)                │
    │◄──────────────────────────────────────┤
```

**Cryptographic details:**
- Ephemeral key exchange: Curve25519 ECDH
- Key derivation: HKDF-SHA512
  - Verify key: `HKDF(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")`
  - Session read key: `HKDF(shared, "Control-Salt", "Control-Read-Encryption-Key")`
  - Session write key: `HKDF(shared, "Control-Salt", "Control-Write-Encryption-Key")`
- Encryption: ChaCha20-Poly1305 (AEAD)
- Signature: Ed25519 over `(ephPK ∥ identifier ∥ peerEphPK)`

#### 3.1.4 Encrypted Session

After pair-verify, all HTTP communication is encrypted frame-by-frame:

```
┌─────────────────┬──────────────────────┬──────────────┐
│ 2-byte LE length │ Ciphertext           │ 16-byte tag  │
│ (plaintext len)  │ (≤1024 bytes plain)  │ (Poly1305)   │
└─────────────────┴──────────────────────┴──────────────┘
```

- Algorithm: ChaCha20-Poly1305
- Nonce: 4 zero bytes + 8-byte little-endian counter (incremented per frame)
- AAD: the 2-byte length field
- Max plaintext per frame: 1024 bytes (larger messages span multiple frames)
- Separate counters and keys for read vs. write directions

#### 3.1.5 Characteristic Access

Over the encrypted session, standard HTTP requests access the accessory database:

| Operation | HTTP Method | Path | Body |
|-----------|------------|------|------|
| List accessories | GET | `/accessories` | — |
| Read characteristic | GET | `/characteristics?id=AID.IID` | — |
| Write characteristic | PUT | `/characteristics` | JSON `{characteristics: [{aid, iid, value}]}` |
| Subscribe to events | PUT | `/characteristics` | JSON `{characteristics: [{aid, iid, ev: true}]}` |

**Event notifications** arrive as `EVENT/1.0 200 OK` responses (not solicited) containing a JSON body with changed characteristic values.

### 3.2 CameraRTPStreamManagement (Service Type `110`)

This is the core HomeKit service for negotiating camera streams.

#### 3.2.1 Characteristics

| IID (per camera) | Type | Name | Access |
|-------------------|------|------|--------|
| Dynamic | `118` | SetupEndpoints | Read/Write |
| Dynamic | `117` | SelectedRTPStreamConfiguration | Write |
| Dynamic | `120` | StreamingStatus | Read |
| Dynamic | `114` | SupportedVideoStreamConfiguration | Read |
| Dynamic | `115` | SupportedAudioStreamConfiguration | Read |

All values are base64-encoded TLV8 blobs.

#### 3.2.2 Stream Setup Sequence

```
Controller                              Camera
    │                                       │
    │  1. PUT SetupEndpoints (TLV8):        │
    │     SessionID, LocalIP, Ports,        │
    │     SRTP Keys/Salts                   │
    ├──────────────────────────────────────►│
    │                                       │
    │  2. GET SetupEndpoints response:      │
    │     Status, RemoteIP, Ports,          │
    │     Camera SRTP Keys/Salts, SSRCs     │
    │◄──────────────────────────────────────┤
    │                                       │
    │  3. PUT SelectedRTPStreamConfig:      │
    │     SessionID, Command=Start,         │
    │     Video params, Audio params        │
    ├──────────────────────────────────────►│
    │                                       │
    │  4. Camera starts sending SRTP        │
    │     to controller's IP:ports          │
    │◄ ═══════════════════════════════ UDP ══┤
```

#### 3.2.3 TLV8 Structure — SetupEndpoints Request

```
Type 0x01 (SessionID):     16 random bytes
Type 0x03 (Address):
  ├─ Type 0x01 (AddrVersion): 0x01 (IPv4)
  ├─ Type 0x02 (Address):     "192.168.1.10" (string)
  ├─ Type 0x03 (VideoPort):   2-byte LE
  └─ Type 0x04 (AudioPort):   2-byte LE
Type 0x04 (VideoCrypto):
  ├─ Type 0x01 (CryptoSuite): 0x00 (AES-CM-128-HMAC-SHA1-80)
  ├─ Type 0x02 (MasterKey):   16 random bytes
  └─ Type 0x03 (MasterSalt):  14 random bytes
Type 0x05 (AudioCrypto):
  └─ (same structure as VideoCrypto)
```

#### 3.2.4 TLV8 Structure — SelectedRTPStreamConfiguration

```
Type 0x01 (Session):
  └─ Type 0x01 (SessionID): 16 bytes
  └─ Type 0x02 (Command):   0x01 (Start)
Type 0x02 (VideoParams):
  ├─ Type 0x01 (Codec):      0x00 (H.264)
  │    ├─ Type 0x01 (Profile): 0x01 (Main)
  │    └─ Type 0x02 (Level):   0x02 (4.0)
  ├─ Type 0x02 (Attributes):
  │    ├─ Type 0x01 (Width):   2-byte LE (1920)
  │    ├─ Type 0x02 (Height):  2-byte LE (1080)
  │    └─ Type 0x03 (FPS):     1 byte (30)
  ├─ Type 0x03 (RTP):
  │    ├─ Type 0x01 (PayloadType):  1 byte (99)
  │    ├─ Type 0x02 (SSRC):         4-byte LE (random)
  │    ├─ Type 0x03 (MaxBitrate):   2-byte LE (2000 kbps)
  │    └─ Type 0x04 (RTCPInterval): 4-byte float32 (0.5s)
  └─ Type 0x04 (reserved?):  absent
Type 0x03 (AudioParams):
  ├─ Type 0x01 (Codec):      0x02 (AAC-ELD)
  ├─ Type 0x02 (Attributes):
  │    ├─ Type 0x01 (RTPTime): 1 byte (30ms)
  │    ├─ Type 0x02 (SampleRate): 0x01 (16kHz)
  │    └─ Type 0x03 (BitRateMode): 0x00 (Variable)
  └─ Type 0x03 (RTP):
       ├─ Type 0x01 (PayloadType): 1 byte (110)
       ├─ Type 0x02 (SSRC):        4-byte LE (random)
       ├─ Type 0x03 (MaxBitrate):  2-byte LE (24 kbps)
       └─ Type 0x04 (RTCPInterval): 4-byte float32 (5.0s)
```

#### 3.2.5 Session Commands

| Value | Command | Description |
|-------|---------|-------------|
| `0x00` | End | Tear down stream |
| `0x01` | Start | Begin streaming |
| `0x02` | Suspend | Pause (camera may continue sending) |
| `0x03` | Resume | Resume after suspend |
| `0x04` | Reconfigure | Change parameters mid-stream |

### 3.3 SRTP/SRTCP

Camera streams use SRTP (RFC 3711) with the following profile:

| Parameter | Value |
|-----------|-------|
| Crypto suite | AES_CM_128_HMAC_SHA1_80 |
| Master key | 16 bytes (128-bit AES) |
| Master salt | 14 bytes |
| Auth tag | 10 bytes (80-bit HMAC-SHA1) |
| Key derivation rate | 0 (default) |

**Port multiplexing (RFC 5761):** Video RTP and RTCP share a single UDP port. The proxy demultiplexes by payload type: PT < 200 → RTP, PT 200–207 → RTCP.

### 3.4 Motion Events (HAP → ONVIF)

**Source:** MotionSensor service (type `85`), MotionDetected characteristic (boolean).

**Subscription mechanism:**
1. Primary: HAP `EVENT` notifications via characteristic subscription
2. Fallback: Polling every 5 seconds (for cameras that don't reliably send events)

**Relay to ONVIF:** `controller.SubscribeMotionSensor()` → callback → `onvifServer.NotifyMotion()` → `PullPointManager.FanOut()` → all active PullPoint subscriptions.

---

## 4. Component Specifications

### 4.1 Configuration (`internal/config/`)

**File format:** YAML

```go
type Config struct {
    LogLevel     string         `yaml:"log_level"`      // "debug"|"info"|"warn"|"error"
    PairingStore string         `yaml:"pairing_store"`   // Relative path to JSON file
    BindAddress  string         `yaml:"bind_address"`    // Override auto-detected IP
    Cameras      []CameraConfig `yaml:"cameras"`
}

type CameraConfig struct {
    Name      string     `yaml:"name"`        // Must match mDNS advertised name
    SetupCode string     `yaml:"setup_code"`  // XXX-XX-XXX format
    DeviceID  string     `yaml:"device_id"`   // Optional MAC-like identifier
    RTSP      RTSPConfig `yaml:"rtsp"`
    Video     VideoConfig `yaml:"video"`
    Audio     AudioConfig `yaml:"audio"`
    ONVIF     ONVIFConfig `yaml:"onvif"`
}
```

**Defaults applied:**

| Field | Default |
|-------|---------|
| `log_level` | `"info"` |
| `pairing_store` | `"./pairings.json"` |
| `rtsp.port` | `8554` |
| `rtsp.path` | `"/live"` |
| `video.width` | `1920` |
| `video.height` | `1080` |
| `video.fps` | `30` |
| `video.max_bitrate` | `2000` (kbps) |
| `audio.enabled` | `true` |
| `audio.codec` | `"aac-eld"` |
| `audio.sample_rate` | `16000` (Hz) |
| `audio.gain` | `512` (~54dB amplification) |
| `onvif.enabled` | `false` |
| `onvif.port` | `8580` |

**Validation:** At least one camera must be configured.

### 4.2 HAP Controller (`internal/hap/controller.go`)

Manages the lifecycle of camera connections.

```go
type Controller struct {
    store      *PairingStore
    logger     *slog.Logger
    bindAddr   string
    devices    map[string]*hkontroller.Device   // mDNS name → device
    verified   map[string]*VerifiedConn          // camera name → verified conn
    charIDs    map[string]*cameraCharIDs         // camera name → characteristic IIDs
    controller *hkontroller.Controller
}
```

**Key operations:**

| Method | Description |
|--------|-------------|
| `Start(ctx)` | Begin mDNS discovery, populate device map |
| `PairCamera(ctx, name, code)` | Pair-setup (if needed) + pair-verify + enumerate characteristics |
| `StartStream(ctx, name, localIP, videoPorts, videoConfig, audioConfig)` | Negotiate SRTP session, return camera's keys/ports/SSRCs |
| `StopStream(ctx, name, sessionID)` | Send End command to camera |
| `SubscribeMotionSensor(ctx, name, callback)` | Subscribe to motion events + fallback polling |
| `Stop()` | Close all connections, stop discovery |

**Characteristic discovery:** After pair-verify, `GetAccessories()` returns the full accessory database. The controller scans for:
- Service type `110` (CameraRTPStreamManagement) → extracts SetupEndpoints and SelectedConfig IIDs
- Service type `85` (MotionSensor) → extracts MotionDetected IID

**HAP UUID normalization:** UUIDs appear in both short (`"110"`) and long (`"00000110-0000-1000-8000-0026BB765291"`) forms. `trimHAPUUID()` normalizes to short form with leading zeros stripped.

### 4.3 Encrypted Connection (`internal/hap/encrypted_conn.go`)

Wraps a `net.Conn` with HAP session encryption.

```go
type EncryptedConn struct {
    conn       net.Conn
    encryptKey [32]byte    // ChaCha20-Poly1305 key for outbound
    decryptKey [32]byte    // ChaCha20-Poly1305 key for inbound
    encryptCnt uint64      // Outbound frame counter (nonce)
    decryptCnt uint64      // Inbound frame counter (nonce)
    readBuf    []byte      // Buffered decrypted data
}
```

Implements `net.Conn`. Max 1024 bytes of plaintext per frame. Large messages are automatically chunked on write and reassembled on read.

### 4.4 HAP Client (`internal/hap/hap_client.go`)

HTTP client over the encrypted connection for accessing characteristics.

```go
type HAPClient struct {
    enc     *EncryptedConn
    reader  *bufio.Reader
    reqMu   sync.Mutex                      // Serialize requests
    respCh  chan *readResult                 // HTTP response channel
    eventCh chan *CharacteristicsResponse    // EVENT notifications
    done    chan struct{}
}
```

**Key design decisions:**
- Single concurrent request enforced by `reqMu` (HAP spec requirement)
- Background `readLoop` goroutine demultiplexes responses vs. events
- `EVENT/1.0` prefix detection (rewritten to `HTTP/1.1` for `http.ReadResponse` compatibility)
- 30-second timeout per request
- Events buffered on `eventCh` (capacity 10)

### 4.5 TLV8 Codec (`internal/hap/tlv8.go`)

Type-Length-Value encoding used throughout HAP.

```go
type TLV8Item struct {
    Type  byte
    Value []byte
}
```

**Fragmentation:** Values exceeding 255 bytes are automatically split across multiple TLV items with the same type byte. The decoder merges consecutive same-type items.

### 4.6 Pairing Store (`internal/hap/pairing_store.go`)

Thread-safe JSON persistence for pairing keys.

```go
type PairingData struct {
    DeviceID       string   // Camera's HAP identifier
    DeviceLTPK     []byte   // Camera's Ed25519 public key
    ControllerLTSK []byte   // Our Ed25519 private key (64 bytes)
    ControllerLTPK []byte   // Our Ed25519 public key (32 bytes)
    ControllerID   string   // "homekit-rtsp-proxy"
    IPAddress      string   // Last known IP
    Port           int      // Last known port
}
```

### 4.7 SRTP Proxy (`internal/stream/proxy.go`)

Decrypts incoming SRTP/SRTCP and dispatches plain RTP packets.

```go
type SRTPProxy struct {
    videoConn, audioConn       *net.UDPConn
    videoDecryptCtx            *srtp.Context      // Camera's keys
    audioDecryptCtx            *srtp.Context      // Camera's keys
    srtcpEncryptCtx            *srtp.Context      // Controller's keys (for RTCP replies)
    audioReturnCtx             *srtp.Context      // Controller's keys (for silence packets)
    onVideoRTP, onAudioRTP     func(*rtp.Packet)  // Callbacks
}
```

**Background goroutines (4 per stream):**

| Goroutine | Interval | Purpose |
|-----------|----------|---------|
| `readVideoLoop` | Continuous | Receive + decrypt SRTP video, demux RTCP |
| `readAudioLoop` | Continuous | Receive + decrypt SRTP audio |
| `rtcpKeepaliveLoop` | 500ms | Send encrypted ReceiverReports to camera |
| `audioReturnLoop` | 20ms | Send encrypted silence packets to camera |

**Why audio return is required:** HomeKit cameras expect bidirectional audio. Without return packets, some cameras stop streaming after a timeout. Silence packets (4 bytes of zeros) satisfy this requirement with negligible bandwidth.

**Why RTCP keepalive is required:** HomeKit cameras expect periodic ReceiverReports (matching the negotiated 0.5s RTCPInterval). Without them, the camera tears down the stream.

### 4.8 RTSP Server (`internal/stream/rtsp_server.go`)

Serves decrypted RTP streams to standard RTSP clients via gortsplib.

```go
type RTSPServer struct {
    server     *gortsplib.Server
    session    *Session
    stream     *gortsplib.ServerStream
    transcoder *AudioTranscoder
    // ...
}
```

#### 4.8.1 SDP Description

**Video track:**
- Codec: H.264, payload type 96
- Packetization mode: 1 (FU-A fragmentation)
- SPS/PPS: Extracted from first in-band STAP-A packet

**Audio track (when enabled):**
- AAC-ELD cameras: Transcoded to AAC-LC, PT 97, `format.Generic` with hardcoded ASC
- Opus cameras: PT 97, `format.Opus` at 48kHz

#### 4.8.2 RTP Sequence/Timestamp Normalization

**Problem:** When the camera stream restarts (on-demand lifecycle), the camera resets sequence numbers and timestamps to arbitrary values. gortsplib uses the initial values for RTP-Info headers, and RTSP clients may reject discontinuities.

**Solution:**

```
Camera seq:     [random_start, +1, +2, ...]  →  Proxy seq:  [0, 1, 2, ...]
Camera ts:      [random_base, +Δ, +Δ, ...]   →  Proxy ts:   [0, +Δ, +Δ, ...]

On stream restart:
  videoTSOffset = lastVideoTS + (wall_clock_gap × 90000)
  Camera ts [new_base, +Δ, ...] → Proxy ts [videoTSOffset, +Δ, ...]
```

This ensures monotonically increasing timestamps across stream restarts.

#### 4.8.3 IDR Frame Caching & Injection

**Problem:** The camera sends SPS/PPS + IDR only at stream start. If ffprobe or a new RTSP client connects after that initial keyframe, it waits indefinitely for the next one (which the camera may never send until the stream restarts).

**Solution:** Cache the initial IDR frame (STAP-A + FU-A fragments) and periodically re-inject it:

1. **Cache:** On receiving a STAP-A (NAL type 24) followed by FU-A fragments (NAL type 28, starting with IDR type 5), buffer all packets until the marker bit.

2. **Inject:** After a client sends PLAY:
   - Wait 50ms for gortsplib's writer goroutine to initialize
   - Inject cached IDR with fresh sequence numbers and estimated timestamps
   - Repeat injection at 4-second intervals (2 additional injections) to cover ffmpeg's ~5-second probe phase

3. **Timestamp estimation:** Use wall-clock time since the last camera packet to compute plausible H.264 timestamps (90kHz clock).

### 4.9 On-Demand Session (`internal/stream/session.go`)

State machine for stream lifecycle:

```
         ClientConnected()         onStart() succeeds
  Idle ──────────────────► Starting ───────────────────► Streaming
   ▲                                                        │
   │         onStop() completes          ClientDisconnected()
   └── Stopping ◄──────────────── (clientCount reaches 0) ◄─┘
```

```go
type Session struct {
    mu          sync.Mutex
    state       SessionState    // Idle | Starting | Streaming | Stopping
    clientCount int
    onStart     func() error    // Negotiate HAP stream, open UDP ports
    onStop      func() error    // Send End command, close proxy
}
```

- `ClientConnected()` increments count; triggers `onStart` on first client
- `ClientDisconnected()` decrements count; triggers `onStop` when count reaches 0

### 4.10 Audio Transcoder (`internal/stream/audio_transcoder.go`)

Transcodes AAC-ELD (HomeKit-only) to AAC-LC (universally supported) using CGo + libfdk-aac.

```go
type AudioTranscoder struct {
    decoder      C.HANDLE_AACDECODER
    encoder      C.HANDLE_AACENCODER
    outASC       []byte          // Encoder's AudioSpecificConfig
    decBuf       []C.INT_PCM     // Decoded PCM buffer (one ELD frame)
    pcmRing      []C.INT_PCM     // PCM accumulator
    pcmLen       int
    encFrameSize int             // 1024 samples (AAC-LC)
}
```

#### 4.10.1 Pipeline

```
AAC-ELD RTP packet
    │
    ▼
Strip 4-byte AU header (RFC 3640)
    │
    ▼
libfdk-aac decoder (AAC-ELD, 512 samples/frame)
    │
    ▼
PCM int16 samples (peak ~7/32767 — very quiet!)
    │
    ▼
Amplify ×gain (configurable, default 512 ≈ +54dB), clamp ±32767
    │
    ▼
Accumulate in ring buffer
    │
    ▼
When ≥1024 samples: libfdk-aac encoder (AAC-LC)
    │
    ▼
Raw AAC-LC bitstream (no AU headers)
    │
    ▼
Prepend 4-byte AU header, renumber seq/ts
    │
    ▼
stream.WritePacketRTP()
```

#### 4.10.2 AAC-ELD AudioSpecificConfig

The `mediacommon/v2` library cannot encode AAC-ELD AudioSpecificConfig correctly — it truncates the extended audio object type (39) to 5 bits, producing type 7 (TwinVQ) instead. The proxy uses a hardcoded hex string.

**Correct ASC for AAC-ELD 16kHz mono:** `F8F02000`

```
Bits 0-4:   11111            Extended escape
Bits 5-10:  000111 (= 39-32) Extended AOT = ELD
Bits 11-14: 1000             Frequency index 8 = 16kHz
Bits 15-18: 0001             Channel config 1 = mono
Bit  19:    0                frameLengthFlag = 512 samples
Bits 20-22: 000              No resilience flags
Bit  23:    0                No LD-SBR
```

#### 4.10.3 Frame Size Mismatch

AAC-ELD produces 512 samples per frame; AAC-LC consumes 1024 samples per frame. Approximately 2 ELD input frames produce 1 LC output frame. The ring buffer absorbs this mismatch, and `Transcode()` returns `nil` (not an error) when insufficient PCM has accumulated.

#### 4.10.4 Gain Compensation

The AAC-ELD decoder output is approximately 54dB below expected levels (peak amplitude ~7 out of 32767). The cause appears to be DRC (Dynamic Range Control) metadata embedded in the camera's AAC-ELD stream. DRC is disabled in the decoder, and a configurable gain factor is applied post-decode with hard clipping at ±32767. The gain defaults to 512 (~54dB) and can be set per-camera via `audio.gain` in the config (0 = passthrough, no amplification).

### 4.11 ONVIF Server (`internal/onvif/`)

Emulates an ONVIF-compatible camera device for Scrypted integration.

#### 4.11.1 SOAP Endpoints

| Path | Actions |
|------|---------|
| `/onvif/device_service` | GetDeviceInformation, GetCapabilities, GetServices, GetSystemDateAndTime, GetScopes |
| `/onvif/media_service` | GetProfiles, GetStreamUri, GetVideoSources, GetVideoEncoderConfiguration, GetSnapshotUri |
| `/onvif/event_service` | GetEventProperties, CreatePullPointSubscription, GetServiceCapabilities |
| `/onvif/event_service/pullpoint/{id}` | PullMessages, Renew, Unsubscribe |

#### 4.11.2 PullPoint Subscriptions

```go
type PullPointSubscription struct {
    ID              string          // UUID
    Created         time.Time
    TerminationTime time.Time
    events          []MotionEvent   // Buffered events
    notify          chan struct{}    // Wake signal for long-poll
}
```

**Long-polling:** `PullMessages()` blocks until either:
- An event arrives (via `notify` channel)
- The timeout expires (returns empty)

**Lifecycle:** Subscriptions auto-expire after their termination time. A cleanup goroutine runs every 60 seconds to remove expired subscriptions.

**Motion event XML:**
```xml
<tt:SimpleItem Name="IsMotion" Value="true"/>
```

---

## 5. Concurrency Model

### 5.1 Goroutine Map (per camera)

| # | Goroutine | Lifetime | Purpose |
|---|-----------|----------|---------|
| 1 | mDNS discovery loop | Process lifetime | Handle discovered/lost devices |
| 2 | HAP event reader | While paired | Read EVENT notifications from camera |
| 3 | Motion poll fallback | While paired | 5s polling if events unreliable |
| 4 | SRTP video reader | While streaming | Decrypt + dispatch video RTP |
| 5 | SRTP audio reader | While streaming | Decrypt + dispatch audio RTP |
| 6 | RTCP keepalive | While streaming | Send ReceiverReports every 500ms |
| 7 | Audio return | While streaming | Send silence packets every 20ms |
| 8 | IDR injector | Per RTSP client | Inject cached keyframes (short-lived) |
| 9 | ONVIF HTTP server | Process lifetime | Handle SOAP requests |
| 10 | ONVIF cleanup | Process lifetime | Expire stale subscriptions every 60s |
| 11 | HAP client readLoop | While connected | Demux responses vs. events |

### 5.2 Synchronization

| Mutex | Protects | Contention Pattern |
|-------|----------|--------------------|
| `Controller.mu` | `devices`, `verified`, `charIDs` maps | Low (setup-time only) |
| `Session.mu` | `state`, `clientCount` | Low (connect/disconnect) |
| `RTSPServer.rtpMu` | Video seq/ts counters | Medium (every video packet + IDR injection) |
| `RTSPServer.idrMu` | IDR cache packets | Low (first IDR + periodic injection) |
| `HAPClient.reqMu` | Request serialization | Low (one request at a time) |
| `PairingStore.mu` | JSON read/write | Low (pair-setup only) |
| `PullPointSubscription.mu` | Event buffer | Low (motion events) |
| `PullPointManager.mu` | Subscription map | Low (create/remove/fanout) |

**Deadlock prevention:** No nested mutex acquisitions. Locks are released before calling potentially-blocking operations (e.g., `stream.WritePacketRTP`).

---

## 6. Error Handling & Resilience

### 6.1 Retry Strategies

| Operation | Retries | Backoff | Failure Mode |
|-----------|---------|---------|--------------|
| HAP pairing | 6 | 10s fixed | Fatal exit |
| HAP pair-verify | 3 | Immediate | Fatal exit |
| SRTP decryption | None | — | Log warn, drop packet |
| Audio transcode | None | — | Return nil (skip frame) |
| RTCP send | None | — | Log warn (non-fatal) |
| ONVIF subscription | None | — | Client retries |

### 6.2 Graceful Shutdown

On SIGINT/SIGTERM:
1. Cancel context → stops mDNS discovery
2. Stop ONVIF HTTP servers
3. Stop RTSP servers (closes client connections)
4. Send End command to camera (via HAP)
5. Close SRTP proxy UDP sockets
6. Close HAP encrypted connections

---

## 7. Deployment

### 7.1 Docker Container

**Multi-stage Dockerfile:**

```dockerfile
# Stage 1: Build (golang:1.25-bookworm + libfdk-aac-dev)
# Stage 2: Runtime (debian:bookworm-slim + libfdk-aac2)
ENTRYPOINT ["/app/homekit-rtsp-proxy", "-config", "config.yaml"]
```

**Docker Compose entry:**
```yaml
homekit-rtsp-proxy:
  container_name: homekit-rtsp-proxy
  build: /home/pi/homekit-rtsp-proxy
  restart: unless-stopped
  network_mode: host                    # Required for mDNS + port exposure
  volumes:
    - /etc/homekit-rtsp-proxy:/app/data:rw
  working_dir: /app/data
  logging:
    driver: "json-file"
    options:
      max-size: "10m"
      max-file: "3"
```

**Host networking rationale:** mDNS (multicast DNS) requires raw access to the network interface for both sending and receiving multicast packets. Bridge networking with port mapping does not forward multicast traffic. Additionally, the camera sends SRTP packets to the controller's advertised IP, which must be a real LAN address.

### 7.2 Persistent State

All mutable state is stored in the mounted volume (`/etc/homekit-rtsp-proxy/`):

| File | Purpose | Created By |
|------|---------|------------|
| `config.yaml` | Camera configuration | User |
| `.hkontroller/keypair` | Controller's Ed25519 identity | `hkontroller` library |
| `.hkontroller/*.pairing` | Per-device pairing data | `hkontroller` library |
| `pairings.json` | IP/port/key cache for reconnection | Application |

### 7.3 Network Ports

| Port | Protocol | Service |
|------|----------|---------|
| 8554 | TCP | RTSP (per camera, configurable) |
| 8580 | TCP/HTTP | ONVIF SOAP (per camera, configurable) |
| Random | UDP | SRTP receive (ephemeral, per stream session) |

### 7.4 Build Requirements

- Go 1.25+
- `libfdk-aac-dev` (build time, for CGo audio transcoder)
- `libfdk-aac2` (runtime)
- Target: `linux/arm64` (Raspberry Pi)

---

## 8. Debug & Test Tools

### 8.1 `cmd/debug-pair/`

Standalone tool for testing HAP pairing and pair-verify independently. Performs mDNS discovery, pair-setup, custom pair-verify, and dumps the full accessory database. Useful for debugging connectivity issues without running the full proxy.

**Usage:** `./debug-pair [device-name] [setup-code]`

### 8.2 `cmd/test-rtsp/`

Minimal RTSP server that sends synthetic H.264 frames (STAP-A + IDR + P-frames) for testing RTSP client compatibility without a real camera.

**Usage:** Listens on `rtsp://localhost:8555/test`, sends 200 frames at 20fps.

---

## 9. Known Limitations & Gotchas

1. **AAC-ELD only:** Aqara Camera E1 cameras reject Opus (error -70410). The codec negotiation is hardcoded to AAC-ELD. Cameras that only support Opus would need code changes.

2. **Single stream per camera:** HomeKit cameras typically support only 1–2 concurrent streams. The proxy uses one. If the camera's stream limit is reached, `StartStream` will fail.

3. **No SPS/PPS in SDP:** The H.264 format is initialized without SPS/PPS (extracted from the first in-band STAP-A). Some strict RTSP clients may reject the initial SDP. The IDR injection mechanism mitigates this.

4. **Audio gain tuning:** The default ×512 gain factor is tuned for the Aqara Camera E1's AAC-ELD output. Other cameras may need different values (configurable via `audio.gain`).

5. **`mediacommon/v2` AAC-ELD bug:** The library cannot correctly encode AAC-ELD AudioSpecificConfig. A hardcoded hex string is used instead of computed SDP parameters.

6. **Pair-verify M1 workaround:** The `hkontroller` library sends a `Method` TLV in pair-verify M1, which violates the HAP spec and causes some cameras to reject the handshake. The custom `DoPairVerify` implementation omits this field.

7. **No stream recovery:** If the camera drops the SRTP stream unexpectedly, the proxy does not automatically re-negotiate. The RTSP client must disconnect and reconnect to trigger a new on-demand session.

8. **Motion event reliability:** Some cameras don't reliably send HAP EVENT notifications. The 5-second polling fallback ensures motion events are eventually detected, but with up to 5 seconds of latency.

---

## 10. Dependencies

| Module | Version | Purpose | License |
|--------|---------|---------|---------|
| `hkontroller` | v0.0.0-20230308 | HAP mDNS discovery, pair-setup | MIT |
| `gortsplib/v5` | v5.3.2 | RTSP server, RTP/RTCP | MIT |
| `mediacommon/v2` | v2.8.0 | Media codec format definitions | MIT |
| `pion/srtp/v3` | v3.0.10 | SRTP context creation, manual decrypt | MIT |
| `pion/rtp` | v1.10.1 | RTP packet parsing/marshalling | MIT |
| `pion/rtcp` | v1.2.16 | RTCP ReceiverReport construction | MIT |
| `golang.org/x/crypto` | v0.48.0 | Ed25519, Curve25519, ChaCha20, HKDF | BSD-3 |
| `google/uuid` | v1.6.0 | ONVIF subscription IDs | BSD-3 |
| `gopkg.in/yaml.v3` | v3.0.1 | YAML config parsing | Apache-2.0 |
| `libfdk-aac` | System | AAC-ELD/LC encode/decode (CGo) | FDK License |
