package stream

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// SRTPProxy receives SRTP packets from a HomeKit camera, decrypts them,
// and forwards them as plain RTP. Unlike pion/srtp's SessionSRTP, this
// implementation manually handles SRTP/SRTCP demuxing (like go2rtc) so
// we can see and respond to RTCP packets from the camera.
type SRTPProxy struct {
	logger *slog.Logger

	videoConn *net.UDPConn
	audioConn *net.UDPConn

	// Manual SRTP decryption contexts (camera's keys, for decrypting incoming).
	videoDecryptCtx *srtp.Context
	audioDecryptCtx *srtp.Context

	// SRTCP encryption context (controller's keys, for encrypting outgoing RTCP).
	srtcpEncryptCtx *srtp.Context

	// Audio return SRTP context (controller's keys, for encrypting outgoing audio).
	audioReturnCtx *srtp.Context

	// Camera's remote addresses.
	cameraAddr      *net.UDPAddr
	cameraAudioAddr *net.UDPAddr

	// SSRCs.
	controllerSSRC  uint32 // our own video SSRC
	cameraVideoSSRC uint32 // camera's video SSRC (discovered from packets)
	audioReturnSSRC uint32 // our own audio SSRC

	// Audio return state.
	audioReturnSeq uint16
	audioReturnTS  uint32

	// Track highest received video seq for ReceptionReport.
	highestVideoSeq uint32

	// Callbacks for forwarding decrypted packets.
	onVideoRTP func(*rtp.Packet)
	onAudioRTP func(*rtp.Packet)

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// SRTPConfig holds the SRTP keys from the SetupEndpoints exchange.
type SRTPConfig struct {
	VideoKey  []byte
	VideoSalt []byte
	AudioKey  []byte
	AudioSalt []byte
	VideoSSRC uint32
	AudioSSRC uint32
	// Camera's address for sending RTCP keepalives and audio return.
	CameraAddr      *net.UDPAddr
	CameraAudioAddr *net.UDPAddr
	// Controller's own SRTP keys and SSRCs (what we sent to the camera).
	// Used for encrypting SRTCP and audio return sent back to the camera.
	ControllerVideoKey  []byte
	ControllerVideoSalt []byte
	ControllerVideoSSRC uint32
	ControllerAudioKey  []byte
	ControllerAudioSalt []byte
	ControllerAudioSSRC uint32
}

// NewSRTPProxy creates a new SRTP-to-RTP proxy.
func NewSRTPProxy(logger *slog.Logger) *SRTPProxy {
	return &SRTPProxy{
		logger: logger,
		stopCh: make(chan struct{}),
	}
}

// SetCallbacks sets the packet forwarding callbacks.
func (p *SRTPProxy) SetCallbacks(onVideo, onAudio func(*rtp.Packet)) {
	p.onVideoRTP = onVideo
	p.onAudioRTP = onAudio
}

// OpenPorts opens UDP listeners on the specified ports (or random ports if 0).
// Returns the actual video and audio ports. Call this before SetupEndpoints.
func (p *SRTPProxy) OpenPorts(videoPort, audioPort int) (int, int, error) {
	var err error

	p.stopCh = make(chan struct{})

	p.videoConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: videoPort})
	if err != nil {
		return 0, 0, fmt.Errorf("listen video UDP: %w", err)
	}

	p.audioConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: audioPort})
	if err != nil {
		p.videoConn.Close()
		p.videoConn = nil
		return 0, 0, fmt.Errorf("listen audio UDP: %w", err)
	}

	actualVideoPort := p.videoConn.LocalAddr().(*net.UDPAddr).Port
	actualAudioPort := p.audioConn.LocalAddr().(*net.UDPAddr).Port

	return actualVideoPort, actualAudioPort, nil
}

// Start sets up SRTP contexts on already-opened ports and begins decrypting.
func (p *SRTPProxy) Start(cfg SRTPConfig) error {
	if p.videoConn == nil || p.audioConn == nil {
		return fmt.Errorf("ports not opened; call OpenPorts first")
	}

	var err error

	// Create SRTP decryption contexts using the camera's keys.
	p.videoDecryptCtx, err = srtp.CreateContext(
		cfg.VideoKey, cfg.VideoSalt,
		srtp.ProtectionProfileAes128CmHmacSha1_80,
	)
	if err != nil {
		p.Close()
		return fmt.Errorf("create video decrypt context: %w", err)
	}

	p.audioDecryptCtx, err = srtp.CreateContext(
		cfg.AudioKey, cfg.AudioSalt,
		srtp.ProtectionProfileAes128CmHmacSha1_80,
	)
	if err != nil {
		p.Close()
		return fmt.Errorf("create audio decrypt context: %w", err)
	}

	// Create SRTCP encryption context using the CONTROLLER's video keys.
	if len(cfg.ControllerVideoKey) > 0 {
		p.srtcpEncryptCtx, err = srtp.CreateContext(
			cfg.ControllerVideoKey, cfg.ControllerVideoSalt,
			srtp.ProtectionProfileAes128CmHmacSha1_80,
		)
		if err != nil {
			p.logger.Warn("failed to create SRTCP encrypt context", "error", err)
		}
	}

	// Create audio return SRTP context using the CONTROLLER's audio keys.
	if len(cfg.ControllerAudioKey) > 0 {
		p.audioReturnCtx, err = srtp.CreateContext(
			cfg.ControllerAudioKey, cfg.ControllerAudioSalt,
			srtp.ProtectionProfileAes128CmHmacSha1_80,
		)
		if err != nil {
			p.logger.Warn("failed to create audio return context", "error", err)
		}
	}

	// Store addresses and SSRCs.
	p.cameraAddr = cfg.CameraAddr
	p.cameraAudioAddr = cfg.CameraAudioAddr
	p.controllerSSRC = cfg.ControllerVideoSSRC
	p.cameraVideoSSRC = cfg.VideoSSRC
	p.audioReturnSSRC = cfg.ControllerAudioSSRC

	// Start goroutines.
	p.wg.Add(4)
	go p.readVideoLoop()
	go p.readAudioLoop()
	go p.rtcpKeepaliveLoop()
	go p.audioReturnLoop()

	p.logger.Info("SRTP proxy started",
		"video_port", p.videoConn.LocalAddr().(*net.UDPAddr).Port,
		"audio_port", p.audioConn.LocalAddr().(*net.UDPAddr).Port,
		"camera_video_ssrc", cfg.VideoSSRC,
		"camera_audio_ssrc", cfg.AudioSSRC,
		"controller_video_ssrc", cfg.ControllerVideoSSRC,
		"controller_audio_ssrc", cfg.ControllerAudioSSRC)

	return nil
}

// readVideoLoop reads raw UDP packets from the video port, demuxes RTP vs RTCP,
// and handles each appropriately.
func (p *SRTPProxy) readVideoLoop() {
	defer p.wg.Done()
	buf := make([]byte, 2048)
	var videoCount, rtcpCount uint64
	lastLogTime := time.Now()

	for {
		n, _, err := p.videoConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-p.stopCh:
			default:
				p.logger.Error("video UDP read error", "error", err)
			}
			p.logger.Info("video read loop exiting", "video_packets", videoCount, "rtcp_packets", rtcpCount)
			return
		}

		if n < 2 {
			continue
		}

		// RFC 5761 demuxing: payload type byte distinguishes RTP from RTCP.
		// RTCP packet types are 200-207, RTP payload types are typically < 128.
		pt := buf[1] & 0x7F // strip marker bit for RTP
		if buf[1] >= 200 && buf[1] <= 207 {
			// SRTCP packet from camera.
			rtcpCount++
			p.handleVideoRTCP(buf[:n])
			continue
		}

		// SRTP packet - decrypt manually.
		header := &rtp.Header{}
		decrypted, err := p.videoDecryptCtx.DecryptRTP(nil, buf[:n], header)
		if err != nil {
			if videoCount == 0 {
				p.logger.Warn("video SRTP decrypt error (first packet)",
					"error", err, "size", n, "pt_byte", pt)
			}
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(decrypted); err != nil {
			p.logger.Warn("video RTP unmarshal error", "error", err)
			continue
		}

		videoCount++

		// Track camera's actual SSRC and highest seq.
		if videoCount == 1 {
			p.cameraVideoSSRC = pkt.Header.SSRC
			p.logger.Info("first video RTP packet received",
				"pt", pkt.Header.PayloadType,
				"ssrc", pkt.Header.SSRC,
				"seq", pkt.Header.SequenceNumber,
				"size", n)
		}
		seq32 := uint32(pkt.Header.SequenceNumber)
		if seq32 > p.highestVideoSeq || videoCount == 1 {
			p.highestVideoSeq = seq32
		}

		// Log every second to track camera packet rate.
		if time.Since(lastLogTime) >= time.Second {
			p.logger.Info("video RTP stats",
				"total_packets", videoCount,
				"rtcp_packets", rtcpCount,
				"seq", pkt.Header.SequenceNumber,
				"ts", pkt.Header.Timestamp)
			lastLogTime = time.Now()
		}

		if p.onVideoRTP != nil {
			p.onVideoRTP(pkt)
		}
	}
}

// handleVideoRTCP processes an SRTCP packet received from the camera on the
// video port. Per go2rtc's implementation, when we receive a SenderReport,
// we immediately respond with a ReceiverReport.
func (p *SRTPProxy) handleVideoRTCP(data []byte) {
	// Decrypt SRTCP using camera's keys.
	header := rtcp.Header{}
	decrypted, err := p.videoDecryptCtx.DecryptRTCP(nil, data, &header)
	if err != nil {
		// Not all RTCP-looking packets will decrypt successfully.
		return
	}

	p.logger.Debug("received RTCP from camera",
		"type", header.Type,
		"length", len(decrypted))

	// Respond to SenderReport with ReceiverReport (like go2rtc).
	if header.Type == rtcp.TypeSenderReport {
		p.sendRTCPKeepalive()
	}
}

// readAudioLoop reads raw UDP packets from the audio port and decrypts them.
func (p *SRTPProxy) readAudioLoop() {
	defer p.wg.Done()
	buf := make([]byte, 2048)
	var count uint64

	for {
		n, _, err := p.audioConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-p.stopCh:
			default:
				p.logger.Error("audio UDP read error", "error", err)
			}
			p.logger.Info("audio read loop exiting", "packets", count)
			return
		}

		if n < 2 {
			continue
		}

		// Skip RTCP packets on audio port.
		if buf[1] >= 200 && buf[1] <= 207 {
			continue
		}

		// Decrypt SRTP.
		header := &rtp.Header{}
		decrypted, err := p.audioDecryptCtx.DecryptRTP(nil, buf[:n], header)
		if err != nil {
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(decrypted); err != nil {
			continue
		}

		count++
		if count == 1 {
			p.logger.Info("first audio RTP packet received",
				"pt", pkt.Header.PayloadType,
				"ssrc", pkt.Header.SSRC,
				"seq", pkt.Header.SequenceNumber,
				"size", n)
		}

		if p.onAudioRTP != nil {
			p.onAudioRTP(pkt)
		}
	}
}

// rtcpKeepaliveLoop sends periodic SRTCP Receiver Reports to the camera.
func (p *SRTPProxy) rtcpKeepaliveLoop() {
	defer p.wg.Done()

	if p.cameraAddr == nil || p.videoConn == nil {
		p.logger.Warn("RTCP keepalive disabled: no camera address")
		return
	}

	p.logger.Info("RTCP keepalive started",
		"cameraAddr", p.cameraAddr,
		"controllerSSRC", p.controllerSSRC,
		"cameraVideoSSRC", p.cameraVideoSSRC)

	// Send first RTCP immediately.
	p.sendRTCPKeepalive()

	// Use 0.5s interval matching the video RTCPInterval we negotiate.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.sendRTCPKeepalive()
		}
	}
}

// sendRTCPKeepalive sends an SRTCP ReceiverReport to the camera.
func (p *SRTPProxy) sendRTCPKeepalive() {
	if p.srtcpEncryptCtx == nil {
		return
	}

	rr := &rtcp.ReceiverReport{
		SSRC: p.controllerSSRC,
		Reports: []rtcp.ReceptionReport{
			{
				SSRC:               p.cameraVideoSSRC,
				LastSequenceNumber: p.highestVideoSeq,
			},
		},
	}
	rrBytes, err := rr.Marshal()
	if err != nil {
		p.logger.Warn("marshal RTCP RR", "error", err)
		return
	}

	encrypted, err := p.srtcpEncryptCtx.EncryptRTCP(nil, rrBytes, nil)
	if err != nil {
		p.logger.Warn("encrypt SRTCP RR", "error", err)
		return
	}

	if _, err := p.videoConn.WriteToUDP(encrypted, p.cameraAddr); err != nil {
		p.logger.Warn("send SRTCP to camera", "error", err)
	}

	p.logger.Debug("sent RTCP keepalive",
		"controllerSSRC", p.controllerSSRC,
		"cameraSSRC", p.cameraVideoSSRC,
		"highestSeq", p.highestVideoSeq,
		"addr", p.cameraAddr)
}

// audioReturnLoop sends periodic silence SRTP packets to the camera's audio
// port. HomeKit cameras expect bidirectional audio - we must send something
// to prevent the camera from stopping the stream.
func (p *SRTPProxy) audioReturnLoop() {
	defer p.wg.Done()

	if p.audioReturnCtx == nil || p.cameraAudioAddr == nil {
		p.logger.Warn("audio return disabled")
		return
	}

	p.logger.Info("audio return started",
		"cameraAudioAddr", p.cameraAudioAddr,
		"ssrc", p.audioReturnSSRC)

	// Send silence every 20ms (50 packets/sec).
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	silencePayload := []byte{0x00, 0x00, 0x00, 0x00}

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.sendAudioReturn(silencePayload)
		}
	}
}

// sendAudioReturn sends a single SRTP audio packet to the camera.
func (p *SRTPProxy) sendAudioReturn(payload []byte) {
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    110,
			SequenceNumber: p.audioReturnSeq,
			Timestamp:      p.audioReturnTS,
			SSRC:           p.audioReturnSSRC,
		},
		Payload: payload,
	}
	p.audioReturnSeq++
	p.audioReturnTS += 320

	raw, err := pkt.Marshal()
	if err != nil {
		return
	}

	encrypted, err := p.audioReturnCtx.EncryptRTP(nil, raw, nil)
	if err != nil {
		return
	}

	p.audioConn.WriteToUDP(encrypted, p.cameraAudioAddr)
}

// Close stops the proxy and releases resources.
func (p *SRTPProxy) Close() {
	select {
	case <-p.stopCh:
		return // Already closed.
	default:
		close(p.stopCh)
	}

	if p.videoConn != nil {
		p.videoConn.Close()
	}
	if p.audioConn != nil {
		p.audioConn.Close()
	}

	p.wg.Wait()
	p.logger.Info("SRTP proxy stopped")
}
