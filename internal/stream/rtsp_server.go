package stream

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// RTSPServer serves decrypted RTP streams to RTSP clients using gortsplib.
type RTSPServer struct {
	logger  *slog.Logger
	server  *gortsplib.Server
	session *Session
	stream  *gortsplib.ServerStream

	mu         sync.Mutex
	listenAddr string
	port       int
	path       string
	desc       *description.Session
	medias     []*description.Media

	videoFormat  *format.H264
	audioFormat  format.Format
	audioPT      uint8 // payload type for audio rewriting
	videoCount   uint64
	gotParams   bool // true once SPS/PPS have been extracted

	// videoWriteMu serializes all video writes (normal + IDR injection) to
	// prevent interleaving. Covers seq/ts assignment through WritePacketRTP.
	// Also protects videoSeq, videoSeqInited, videoTSBase, videoTSOffset,
	// lastVideoTS, lastVideoTime.
	videoWriteMu   sync.Mutex
	videoSeq       uint16    // monotonic output sequence counter
	videoSeqInited bool      // true after first video packet in current camera session
	videoTSBase    uint32    // camera's starting timestamp for current session
	videoTSOffset  uint32    // our timestamp offset (accumulated from previous sessions)
	lastVideoTS    uint32    // last real camera packet's output timestamp
	lastVideoTime  time.Time // wall-clock time of last video packet

	// IDR frame cache: the camera only sends SPS/PPS + IDR at stream start,
	// so we cache those packets and replay them for late-joining clients.
	idrMu         sync.Mutex
	idrCache      []*rtp.Packet // complete cached IDR frame (STAP-A + FU-A fragments)
	idrPending    []*rtp.Packet // IDR being collected (not yet complete)
	idrReady      chan struct{} // closed when first IDR is cached
	collectingIDR bool          // true while collecting IDR FU-A fragments

	// Audio transcoder: AAC-ELD → AAC-LC (nil if audio disabled or non-ELD codec).
	transcoder *AudioTranscoder
	audioSeq   uint16 // monotonic audio seq counter (transcoding changes packet count)

	// Snapshotter: decodes the cached IDR into a JPEG kept in memory for
	// the ONVIF /snapshot.jpg endpoint. Refreshed each time a new IDR is
	// fully assembled in the cache.
	snapshotter *Snapshotter
}

// RTSPServerConfig configures the RTSP server.
type RTSPServerConfig struct {
	ListenAddress string // "" = all interfaces, "127.0.0.1" = local only
	Port          int
	Path          string
	HasAudio      bool
	AudioCodec    string // "aac-eld" or "opus"
	SampleRate    int
	AudioGain     int // PCM gain factor for AAC-ELD→AAC-LC transcoding
}

// NewRTSPServer creates a new RTSP server for a single camera.
func NewRTSPServer(cfg RTSPServerConfig, session *Session, logger *slog.Logger) *RTSPServer {
	snap, err := NewSnapshotter(logger)
	if err != nil {
		// Non-fatal: snapshots just won't be available.
		logger.Warn("failed to init snapshotter, /snapshot.jpg will be unavailable", "error", err)
	}

	s := &RTSPServer{
		logger:      logger,
		session:     session,
		listenAddr:  cfg.ListenAddress,
		port:        cfg.Port,
		path:        cfg.Path,
		idrReady:    make(chan struct{}),
		snapshotter: snap,
	}

	// Build SDP with H.264 video.
	// Use PT 96 in SDP; incoming packets from camera use PT 99 but we
	// rewrite them before forwarding.
	s.videoFormat = &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
	}

	videoMedia := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{s.videoFormat},
	}
	s.medias = []*description.Media{videoMedia}

	// Add audio if enabled.
	if cfg.HasAudio {
		sampleRate := cfg.SampleRate
		if sampleRate == 0 {
			sampleRate = 16000
		}

		switch cfg.AudioCodec {
		case "opus":
			// Opus: universally supported by VLC, ffmpeg, browsers.
			// RTP clock rate is always 48kHz per RFC 7587.
			opusFmt := &format.Opus{
				PayloadTyp: 97,
			}
			s.audioFormat = opusFmt
			s.audioPT = 97
		default: // "aac-eld"
			// Transcode AAC-ELD → AAC-LC so that standard decoders (ffmpeg,
			// VLC, Scrypted) can handle the audio without FDK-AAC.
			eldASC := "F8F02000" // 16kHz mono ELD
			if sampleRate == 24000 {
				eldASC = "F8EC2000"
			} else if sampleRate == 8000 {
				eldASC = "F8F82000"
			}

			transcoder, err := NewAudioTranscoder(sampleRate, eldASC, cfg.AudioGain)
			if err != nil {
				logger.Error("failed to create audio transcoder, audio disabled", "error", err)
				break
			}
			s.transcoder = transcoder

			// Use encoder's AAC-LC AudioSpecificConfig for SDP.
			aacConfig := strings.ToUpper(hex.EncodeToString(transcoder.AudioSpecificConfig()))
			logger.Info("audio transcoder initialized",
				"codec", "AAC-ELD → AAC-LC",
				"sampleRate", sampleRate,
				"ascHex", aacConfig)

			genFmt := &format.Generic{
				PayloadTyp: 97,
				RTPMa:      fmt.Sprintf("mpeg4-generic/%d/1", sampleRate),
				FMT: map[string]string{
					"streamtype":       "5",
					"profile-level-id": "1",
					"mode":             "AAC-hbr",
					"sizelength":       "13",
					"indexlength":      "3",
					"indexdeltalength": "3",
					"config":           aacConfig,
				},
			}
			if err := genFmt.Init(); err != nil {
				logger.Warn("failed to init audio format", "error", err)
			}
			s.audioFormat = genFmt
			s.audioPT = 97
		}

		audioMedia := &description.Media{
			Type:    description.MediaTypeAudio,
			Formats: []format.Format{s.audioFormat},
		}
		s.medias = append(s.medias, audioMedia)
	}

	s.desc = &description.Session{
		Medias: s.medias,
	}

	return s
}

// Start begins listening for RTSP connections.
func (s *RTSPServer) Start() error {
	s.server = &gortsplib.Server{
		Handler:        s,
		RTSPAddress:    fmt.Sprintf("%s:%d", s.listenAddr, s.port),
		WriteQueueSize: 2048,
		WriteTimeout:   30 * time.Second, // survive ffmpeg's 5s probe phase
	}

	if err := s.server.Start(); err != nil {
		return fmt.Errorf("start RTSP server: %w", err)
	}

	// gortsplib v5: create ServerStream with struct fields + Initialize().
	s.stream = &gortsplib.ServerStream{
		Server: s.server,
		Desc:   s.desc,
	}
	if err := s.stream.Initialize(); err != nil {
		s.server.Close()
		return fmt.Errorf("initialize server stream: %w", err)
	}

	s.logger.Info("RTSP server started", "port", s.port, "path", s.path)
	return nil
}

// clonePacket creates a deep copy of an RTP packet.
func clonePacket(pkt *rtp.Packet) *rtp.Packet {
	clone := &rtp.Packet{Header: pkt.Header}
	clone.Payload = make([]byte, len(pkt.Payload))
	copy(clone.Payload, pkt.Payload)
	return clone
}

// cacheIDRPacket buffers RTP packets belonging to the IDR access unit.
// The camera sends: STAP-A (SPS+PPS) → FU-A start (IDR) → FU-A middle... → FU-A end (marker=1).
// We collect all of these and store them as the IDR cache.
func (s *RTSPServer) cacheIDRPacket(pkt *rtp.Packet) {
	if len(pkt.Payload) < 1 {
		return
	}

	naluType := pkt.Payload[0] & 0x1F

	s.idrMu.Lock()
	defer s.idrMu.Unlock()

	// STAP-A containing SPS+PPS: start a new IDR collection.
	if naluType == 24 {
		s.idrPending = []*rtp.Packet{clonePacket(pkt)}
		s.collectingIDR = false // will become true when we see IDR FU-A start
		return
	}

	// FU-A packet: check if it's an IDR fragment.
	if naluType == 28 && len(pkt.Payload) > 1 {
		startBit := pkt.Payload[1] & 0x80
		origType := pkt.Payload[1] & 0x1F

		if startBit != 0 && origType == 5 {
			// FU-A start of IDR: begin collecting fragments.
			s.collectingIDR = true
			s.idrPending = append(s.idrPending, clonePacket(pkt))
			return
		}

		if s.collectingIDR {
			s.idrPending = append(s.idrPending, clonePacket(pkt))

			// Marker bit means end of the IDR frame.
			if pkt.Header.Marker {
				s.idrCache = s.idrPending
				s.idrPending = nil
				s.collectingIDR = false
				s.logger.Info("IDR frame cached", "packets", len(s.idrCache))

				// Signal first IDR is ready.
				select {
				case <-s.idrReady:
				default:
					close(s.idrReady)
				}

				// Refresh the snapshot JPEG off the lock-holding path.
				if s.snapshotter != nil && s.gotParams {
					sps, pps := s.videoFormat.SafeParams()
					cache := s.idrCache
					go s.refreshSnapshot(cache, sps, pps)
				}
			}
			return
		}
	}
}

// refreshSnapshot reassembles the cached IDR packets into Annex-B and
// asks the Snapshotter to decode them into a fresh JPEG. Runs off the
// hot RTP path.
func (s *RTSPServer) refreshSnapshot(idrCache []*rtp.Packet, sps, pps []byte) {
	annexB := BuildIDRAnnexB(idrCache, sps, pps)
	if err := s.snapshotter.Update(annexB); err != nil {
		s.logger.Warn("snapshot refresh failed", "error", err)
	}
}

// LatestSnapshotJPEG returns the most recently encoded JPEG snapshot, or
// nil if no IDR has been decoded yet. Implements onvif.SnapshotProvider.
func (s *RTSPServer) LatestSnapshotJPEG() []byte {
	if s.snapshotter == nil {
		return nil
	}
	return s.snapshotter.LatestJPEG()
}

// ResetVideoRTP must be called when a new camera stream session starts.
// It adjusts the timestamp offset to account for wall-clock time elapsed since
// the last packet, keeping gortsplib's RTP-Info prediction aligned.
// It also clears the IDR cache since those packets have stale timestamps.
func (s *RTSPServer) ResetVideoRTP() {
	s.videoWriteMu.Lock()
	if s.videoSeqInited {
		elapsed := time.Since(s.lastVideoTime)
		gap := uint32(elapsed.Seconds() * 90000)
		if gap < 4500 {
			gap = 4500
		}
		s.videoTSOffset = s.lastVideoTS + gap
	}
	s.videoSeqInited = false
	s.videoWriteMu.Unlock()

	// Clear stale IDR cache — the camera will send a fresh IDR on the new session.
	s.idrMu.Lock()
	s.idrCache = nil
	s.idrPending = nil
	s.collectingIDR = false
	s.idrReady = make(chan struct{})
	s.idrMu.Unlock()

	s.logger.Info("video RTP state reset for new camera session",
		"nextTSOffset", s.videoTSOffset, "nextSeqStart", s.videoSeq)
}

// normalizeVideoRTP remaps the camera's seq/timestamp to continuous output values.
// Sequence numbers use a simple monotonic counter. Timestamps are offset-based
// to remain continuous across camera restarts.
// Caller must hold s.videoWriteMu.
func (s *RTSPServer) normalizeVideoRTP(pkt *rtp.Packet) {
	cameraTS := pkt.Header.Timestamp

	if !s.videoSeqInited {
		s.videoTSBase = cameraTS
		s.videoSeqInited = true
		s.logger.Info("video RTP base initialized",
			"cameraTS", cameraTS,
			"outputSeqStart", s.videoSeq, "tsOffset", s.videoTSOffset)
	}

	// Monotonic sequence counter (never reset, always increasing).
	pkt.Header.SequenceNumber = s.videoSeq
	s.videoSeq++

	// Timestamp: offset + (camera_ts - camera_base)
	pkt.Header.Timestamp = s.videoTSOffset + (cameraTS - s.videoTSBase)

	s.lastVideoTS = pkt.Header.Timestamp
	s.lastVideoTime = time.Now()
}

// CacheOnlyVideoPacket receives an H.264 RTP packet for IDR cache purposes only,
// without forwarding it to RTSP clients. Used for gapped IDR frames: the packet
// sequence gaps make them unsafe for live streaming but the payload is still
// valid for populating the IDR cache so late-joining clients can start playing.
// The IDR injection path re-assigns sequence numbers and timestamps at play time,
// so gaps in the cached packets don't corrupt the re-sent IDR.
func (s *RTSPServer) CacheOnlyVideoPacket(pkt *rtp.Packet) {
	// Extract SPS/PPS from STAP-A if not yet seen — needed for snapshot JPEG.
	if !s.gotParams {
		s.extractSPSPPS(pkt.Payload)
	}
	// cacheIDRPacket acquires idrMu internally.
	s.cacheIDRPacket(pkt)
}

// WriteVideoPacket writes a decrypted H.264 RTP packet to all connected clients.
func (s *RTSPServer) WriteVideoPacket(pkt *rtp.Packet) {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()

	if stream == nil {
		return
	}

	// Rewrite payload type to match SDP (camera sends PT 99, SDP has PT 96).
	pkt.Header.PayloadType = s.videoFormat.PayloadType()

	// Extract SPS/PPS from the stream if we haven't yet.
	if !s.gotParams {
		s.extractSPSPPS(pkt.Payload)
	}

	// Hold videoWriteMu for normalize + cache + write to prevent interleaving
	// with IDR injection. WritePacketRTP is non-blocking (pushes to per-client
	// ring buffers), so holding the mutex through the write is safe.
	s.videoWriteMu.Lock()

	s.normalizeVideoRTP(pkt)

	// Cache IDR frame packets for late-joining clients (after normalization).
	// cacheIDRPacket acquires idrMu internally.
	s.cacheIDRPacket(pkt)

	s.videoCount++

	if s.videoCount <= 5 || s.videoCount%500 == 0 {
		s.logger.Debug("writing video RTP to RTSP",
			"count", s.videoCount,
			"seq", pkt.Header.SequenceNumber,
			"ts", pkt.Header.Timestamp,
			"size", len(pkt.Payload),
			"marker", pkt.Header.Marker)
	}

	if err := stream.WritePacketRTP(s.medias[0], pkt); err != nil {
		s.logger.Warn("WritePacketRTP error", "count", s.videoCount, "seq", pkt.Header.SequenceNumber, "error", err)
	}

	s.videoWriteMu.Unlock()
}

// extractSPSPPS parses H.264 RTP payloads to find SPS (NALU type 7)
// and PPS (NALU type 8). Once both are found, it updates the H264 format
// so that subsequent DESCRIBE responses include sprop-parameter-sets.
func (s *RTSPServer) extractSPSPPS(payload []byte) {
	if len(payload) < 1 {
		return
	}

	var sps, pps []byte

	naluType := payload[0] & 0x1F
	switch {
	case naluType == 7: // SPS single NALU
		sps = make([]byte, len(payload))
		copy(sps, payload)
	case naluType == 8: // PPS single NALU
		pps = make([]byte, len(payload))
		copy(pps, payload)
	case naluType == 24: // STAP-A: may contain SPS+PPS bundled
		sps, pps = parseSTAPA(payload)
	default:
		return
	}

	// Merge with any previously found params.
	existingSPS, existingPPS := s.videoFormat.SafeParams()
	if sps != nil {
		existingSPS = sps
	}
	if pps != nil {
		existingPPS = pps
	}

	if existingSPS != nil && existingPPS != nil {
		s.videoFormat.SafeSetParams(existingSPS, existingPPS)
		s.gotParams = true
		s.logger.Info("H.264 SPS/PPS extracted from stream",
			"sps_len", len(existingSPS),
			"pps_len", len(existingPPS))
	}
}

// parseSTAPA extracts SPS and PPS NALUs from a STAP-A aggregate packet.
// STAP-A format: [header byte] [2-byte size] [NALU] [2-byte size] [NALU] ...
func parseSTAPA(payload []byte) (sps, pps []byte) {
	offset := 1 // skip STAP-A header byte
	for offset+2 < len(payload) {
		naluSize := int(binary.BigEndian.Uint16(payload[offset:]))
		offset += 2
		if offset+naluSize > len(payload) {
			break
		}
		nalu := payload[offset : offset+naluSize]
		if len(nalu) > 0 {
			switch nalu[0] & 0x1F {
			case 7: // SPS
				sps = make([]byte, len(nalu))
				copy(sps, nalu)
			case 8: // PPS
				pps = make([]byte, len(nalu))
				copy(pps, nalu)
			}
		}
		offset += naluSize
	}
	return
}

// WriteAudioPacket writes a decrypted audio RTP packet to all connected clients.
// If a transcoder is active, it strips the AU header, transcodes AAC-ELD → AAC-LC,
// and re-adds an AU header with the new frame size.
func (s *RTSPServer) WriteAudioPacket(pkt *rtp.Packet) {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()

	if stream == nil || len(s.medias) < 2 {
		return
	}

	// Rewrite payload type to match SDP (camera sends PT 110, SDP has PT 97).
	pkt.Header.PayloadType = s.audioPT

	if s.transcoder != nil && len(pkt.Payload) > 4 {
		// Strip 4-byte RFC 3640 AU header to get raw AAC-ELD frame.
		rawELD := pkt.Payload[4:]

		rawLC, err := s.transcoder.Transcode(rawELD)
		if err != nil {
			s.logger.Warn("audio transcode error", "error", err)
			return
		}
		if rawLC == nil {
			// Accumulating PCM — not enough for an LC frame yet.
			return
		}

		// Build new 4-byte AU header for the AAC-LC frame.
		// Format: 2 bytes AU-headers-length (16 bits = count of AU header bits),
		//         2 bytes AU header (13-bit size + 3-bit index).
		auHeaderLen := uint16(16) // one AU header = 16 bits
		auHeader := uint16(len(rawLC)) << 3 // 13-bit size, index=0

		newPayload := make([]byte, 4+len(rawLC))
		binary.BigEndian.PutUint16(newPayload[0:2], auHeaderLen)
		binary.BigEndian.PutUint16(newPayload[2:4], auHeader)
		copy(newPayload[4:], rawLC)

		pkt.Payload = newPayload

		// Renumber sequence: transcoding merges ~2 ELD packets into 1 LC
		// packet, so we must emit continuous seq numbers (gaps confuse VLC).
		pkt.Header.SequenceNumber = s.audioSeq
		s.audioSeq++

		// Fix timestamp: each LC frame = 1024 samples at the RTP clock rate.
		// Use a simple counter so timestamps are perfectly monotonic.
		pkt.Header.Timestamp = uint32(s.audioSeq-1) * 1024

		pkt.Header.Marker = true // each packet is a complete AU
	}

	stream.WritePacketRTP(s.medias[1], pkt)
}

// Stop shuts down the RTSP server.
func (s *RTSPServer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.transcoder != nil {
		s.transcoder.Close()
		s.transcoder = nil
	}

	if s.snapshotter != nil {
		s.snapshotter.Close()
		s.snapshotter = nil
	}

	if s.stream != nil {
		s.stream.Close()
		s.stream = nil
	}
	if s.server != nil {
		s.server.Close()
	}
	s.logger.Info("RTSP server stopped", "port", s.port)
}

// gortsplib.ServerHandler implementation

func (s *RTSPServer) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	s.logger.Info("RTSP connection opened", "remote", ctx.Conn.NetConn().RemoteAddr())
}

func (s *RTSPServer) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	s.logger.Info("RTSP connection closed", "remote", ctx.Conn.NetConn().RemoteAddr())
}

func (s *RTSPServer) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	s.logger.Debug("RTSP session opened")
}

func (s *RTSPServer) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.logger.Debug("RTSP session closed")
	if err := s.session.ClientDisconnected(); err != nil {
		s.logger.Error("session client disconnected error", "error", err)
	}

	if s.session.State() == StateIdle {
		s.logger.Info("stream idle (no more clients)")
	}
}

func (s *RTSPServer) OnStreamWriteError(ctx *gortsplib.ServerHandlerOnStreamWriteErrorCtx) {
	s.logger.Warn("stream write error (queue full or TCP stall)",
		"error", ctx.Error)
}

func (s *RTSPServer) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{
		StatusCode: base.StatusOK,
	}, s.stream, nil
}

func (s *RTSPServer) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{
		StatusCode: base.StatusOK,
	}, s.stream, nil
}

func (s *RTSPServer) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	// Start the camera asynchronously. We must return from OnPlay before
	// gortsplib will register this client as an active reader. If we start
	// the camera synchronously (~170ms HAP exchange), the first RTP packets
	// (STAP-A with SPS/PPS + IDR frame) arrive before the reader is
	// registered and get silently dropped.
	rtspSession := ctx.Session
	go func() {
		// Wait for gortsplib to finish initializing the TCP writer.
		// The writer is started asynchronously after the PLAY response is sent.
		time.Sleep(100 * time.Millisecond)

		freshStart, err := s.session.ClientConnected()
		if err != nil {
			s.logger.Error("failed to start stream for client", "error", err)
			rtspSession.Close()
			return
		}

		// Wait for the IDR cache to be populated.
		if freshStart {
			s.logger.Info("fresh camera start, waiting for IDR cache")
		} else {
			s.logger.Info("additional client, waiting for IDR cache")
		}
		select {
		case <-s.idrReady:
		case <-time.After(10 * time.Second):
			// No IDR arrived in time (likely heavy packet loss dropped all
			// keyframes). Close the RTSP session so the client reconnects and
			// triggers a proper ClientDisconnected cleanup, rather than leaving
			// a zombie client that never receives video.
			s.logger.Warn("timeout waiting for IDR cache in OnPlay, closing client session")
			rtspSession.Close()
			return
		}

		// Inject cached IDR so the client can start decoding immediately.
		// WritePacketRTP is non-blocking (queues to ring buffer), so holding
		// videoWriteMu for the batch takes <1ms and doesn't block the read loop.
		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()
		if stream == nil {
			return
		}

		s.videoWriteMu.Lock()
		s.idrMu.Lock()
		cache := s.idrCache
		s.idrMu.Unlock()
		if len(cache) > 0 {
			idrTS := s.lastVideoTS + 1
			startSeq := s.videoSeq
			for _, cached := range cache {
				clone := clonePacket(cached)
				clone.Header.SequenceNumber = s.videoSeq
				s.videoSeq++
				clone.Header.Timestamp = idrTS
				clone.Header.PayloadType = s.videoFormat.PayloadType()
				stream.WritePacketRTP(s.medias[0], clone)
			}
			s.logger.Info("IDR injected for client",
				"packets", len(cache), "startSeq", startSeq, "ts", idrTS)
		}
		s.videoWriteMu.Unlock()
	}()

	return &base.Response{
		StatusCode: base.StatusOK,
		Header: base.Header{
			"Range": base.HeaderValue{"npt=0.000-"},
		},
	}, nil
}


