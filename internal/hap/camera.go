package hap

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"net"
)

// HomeKit CameraRTPStreamManagement TLV8 types.
// See HAP specification §9.16 and Apple's HomeKit ADK.

// Top-level TLV types for SetupEndpoints
const (
	TLVSessionID        = 0x01
	TLVStatus           = 0x02
	TLVAddress          = 0x03
	TLVVideoParams      = 0x04
	TLVAudioParams      = 0x05
	TLVSRTPCryptoSuite  = 0x06
)

// Address sub-TLV types
const (
	TLVAddressVersion = 0x01
	TLVAddressIP      = 0x02
	TLVAddressVideoPort = 0x03
	TLVAddressAudioPort = 0x04
)

// SRTP parameter sub-TLV types
const (
	TLVSRTPCryptoType   = 0x01
	TLVSRTPMasterKey    = 0x02
	TLVSRTPMasterSalt   = 0x03
)

// Selected RTP Stream Configuration TLV types
const (
	TLVSelectedSessionControl = 0x01
	TLVSelectedVideoParams    = 0x02
	TLVSelectedAudioParams    = 0x03
)

// Session control sub-TLV types
const (
	TLVSessionControlID      = 0x01
	TLVSessionControlCommand = 0x02
)

// Session commands
const (
	SessionCommandEnd       = 0x00
	SessionCommandStart     = 0x01
	SessionCommandSuspend   = 0x02
	SessionCommandResume    = 0x03
	SessionCommandReconfigure = 0x04
)

// Video parameter sub-TLV types
const (
	TLVVideoCodecType    = 0x01
	TLVVideoCodecParams  = 0x02
	TLVVideoAttributes   = 0x03
	TLVVideoRTPParams    = 0x04
)

// Video codec types
const (
	VideoCodecH264 = 0x00
)

// H.264 profile IDs
const (
	H264ProfileBaseline = 0x00
	H264ProfileMain     = 0x01
	H264ProfileHigh     = 0x02
)

// H.264 levels
const (
	H264Level3_1 = 0x00
	H264Level3_2 = 0x01
	H264Level4_0 = 0x02
)

// Audio parameter sub-TLV types
const (
	TLVAudioCodecType   = 0x01
	TLVAudioCodecParams = 0x02
	TLVAudioRTPParams   = 0x03
	TLVAudioComfort     = 0x04
)

// Audio codec types
const (
	AudioCodecPCMU   = 0x00
	AudioCodecPCMA   = 0x01
	AudioCodecAACELD = 0x02
	AudioCodecOpus   = 0x03
)

// Video codec param sub-TLV types
const (
	TLVVideoProfileID         = 0x01
	TLVVideoLevel             = 0x02
	TLVVideoPacketizationMode = 0x03
)

// Audio codec param sub-TLV types
const (
	TLVAudioChannels   = 0x01
	TLVAudioBitRate    = 0x02
	TLVAudioSampleRate = 0x03
	TLVAudioPacketTime = 0x04
)

// Audio sample rates
const (
	AudioSampleRate8kHz  = 0x00
	AudioSampleRate16kHz = 0x01
	AudioSampleRate24kHz = 0x02
)

// RTP parameter sub-TLV types
const (
	TLVRTPPayloadType       = 0x01
	TLVRTPSSRC              = 0x02
	TLVRTPMaxBitrate        = 0x03
	TLVRTPMinInterval       = 0x04
	TLVRTPMaxMTU            = 0x05
)

// Streaming status TLV types
const (
	TLVStreamingStatus = 0x01
)

// Status values
const (
	StreamingStatusAvailable   = 0x00
	StreamingStatusInUse       = 0x01
	StreamingStatusUnavailable = 0x02
)

// CryptoSuite values
const (
	CryptoSuiteAES_CM_128_HMAC_SHA1_80 = 0x00
	CryptoSuiteAES_CM_256_HMAC_SHA1_80 = 0x01
	CryptoSuiteDisabled                = 0x02
)

// StreamEndpoints holds the parameters for a stream setup exchange.
type StreamEndpoints struct {
	SessionID    [16]byte
	LocalIP      net.IP
	LocalVideoPort  uint16
	LocalAudioPort  uint16
	VideoSRTPKey    [16]byte
	VideoSRTPSalt   [14]byte
	AudioSRTPKey    [16]byte
	AudioSRTPSalt   [14]byte
}

// StreamResponse holds the camera's response to SetupEndpoints.
type StreamResponse struct {
	SessionID      [16]byte
	Status         byte
	RemoteIP       net.IP
	RemoteVideoPort uint16
	RemoteAudioPort uint16
	VideoSRTPKey    []byte
	VideoSRTPSalt   []byte
	AudioSRTPKey    []byte
	AudioSRTPSalt   []byte
	VideoSSRC       uint32
	AudioSSRC       uint32

	// Controller's own keys and SSRCs (what we sent to the camera).
	// Needed for encrypting outbound SRTCP and audio return.
	ControllerVideoKey   []byte
	ControllerVideoSalt  []byte
	ControllerVideoSSRC  uint32
	ControllerAudioKey   []byte
	ControllerAudioSalt  []byte
	ControllerAudioSSRC  uint32
}

// GenerateStreamEndpoints creates a new stream setup request with random keys.
func GenerateStreamEndpoints(localIP net.IP, videoPort, audioPort uint16) (*StreamEndpoints, error) {
	ep := &StreamEndpoints{
		LocalIP:        localIP,
		LocalVideoPort: videoPort,
		LocalAudioPort: audioPort,
	}

	if _, err := rand.Read(ep.SessionID[:]); err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}
	if _, err := rand.Read(ep.VideoSRTPKey[:]); err != nil {
		return nil, fmt.Errorf("generate video SRTP key: %w", err)
	}
	if _, err := rand.Read(ep.VideoSRTPSalt[:]); err != nil {
		return nil, fmt.Errorf("generate video SRTP salt: %w", err)
	}
	if _, err := rand.Read(ep.AudioSRTPKey[:]); err != nil {
		return nil, fmt.Errorf("generate audio SRTP key: %w", err)
	}
	if _, err := rand.Read(ep.AudioSRTPSalt[:]); err != nil {
		return nil, fmt.Errorf("generate audio SRTP salt: %w", err)
	}

	return ep, nil
}

// EncodeSetupEndpoints encodes the setup request as TLV8.
func EncodeSetupEndpoints(ep *StreamEndpoints) []byte {
	ipStr := ep.LocalIP.String()
	addrVersion := byte(0x00) // IPv4
	if ep.LocalIP.To4() == nil {
		addrVersion = 0x01 // IPv6
	}

	videoPortBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(videoPortBytes, ep.LocalVideoPort)
	audioPortBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(audioPortBytes, ep.LocalAudioPort)

	addressTLV := TLV8Encode([]TLV8Item{
		{Type: TLVAddressVersion, Value: []byte{addrVersion}},
		{Type: TLVAddressIP, Value: []byte(ipStr)},
		{Type: TLVAddressVideoPort, Value: videoPortBytes},
		{Type: TLVAddressAudioPort, Value: audioPortBytes},
	})

	videoSRTP := TLV8Encode([]TLV8Item{
		{Type: TLVSRTPCryptoType, Value: []byte{CryptoSuiteAES_CM_128_HMAC_SHA1_80}},
		{Type: TLVSRTPMasterKey, Value: ep.VideoSRTPKey[:]},
		{Type: TLVSRTPMasterSalt, Value: ep.VideoSRTPSalt[:]},
	})

	audioSRTP := TLV8Encode([]TLV8Item{
		{Type: TLVSRTPCryptoType, Value: []byte{CryptoSuiteAES_CM_128_HMAC_SHA1_80}},
		{Type: TLVSRTPMasterKey, Value: ep.AudioSRTPKey[:]},
		{Type: TLVSRTPMasterSalt, Value: ep.AudioSRTPSalt[:]},
	})

	return TLV8Encode([]TLV8Item{
		{Type: TLVSessionID, Value: ep.SessionID[:]},
		{Type: TLVAddress, Value: addressTLV},
		{Type: TLVVideoParams, Value: videoSRTP},
		{Type: TLVAudioParams, Value: audioSRTP},
	})
}

// DecodeStreamResponse decodes the camera's SetupEndpoints response.
func DecodeStreamResponse(data []byte) (*StreamResponse, error) {
	items, err := TLV8Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode response TLV8: %w", err)
	}

	resp := &StreamResponse{}

	sessionID := TLV8GetBytes(items, TLVSessionID)
	if len(sessionID) == 16 {
		copy(resp.SessionID[:], sessionID)
	}

	if status, ok := TLV8GetByte(items, TLVStatus); ok {
		resp.Status = status
	}

	// Decode address
	addrData := TLV8GetBytes(items, TLVAddress)
	if addrData != nil {
		addrItems, err := TLV8Decode(addrData)
		if err == nil {
			if ip := TLV8GetBytes(addrItems, TLVAddressIP); ip != nil {
				resp.RemoteIP = net.ParseIP(string(ip))
			}
			if vp := TLV8GetBytes(addrItems, TLVAddressVideoPort); len(vp) == 2 {
				resp.RemoteVideoPort = binary.LittleEndian.Uint16(vp)
			}
			if ap := TLV8GetBytes(addrItems, TLVAddressAudioPort); len(ap) == 2 {
				resp.RemoteAudioPort = binary.LittleEndian.Uint16(ap)
			}
		}
	}

	// Decode video SRTP params
	videoData := TLV8GetBytes(items, TLVVideoParams)
	if videoData != nil {
		videoItems, err := TLV8Decode(videoData)
		if err == nil {
			resp.VideoSRTPKey = TLV8GetBytes(videoItems, TLVSRTPMasterKey)
			resp.VideoSRTPSalt = TLV8GetBytes(videoItems, TLVSRTPMasterSalt)
		}
	}

	// Decode audio SRTP params
	audioData := TLV8GetBytes(items, TLVAudioParams)
	if audioData != nil {
		audioItems, err := TLV8Decode(audioData)
		if err == nil {
			resp.AudioSRTPKey = TLV8GetBytes(audioItems, TLVSRTPMasterKey)
			resp.AudioSRTPSalt = TLV8GetBytes(audioItems, TLVSRTPMasterSalt)
		}
	}

	return resp, nil
}

// EncodeSelectedStreamConfig builds the SelectedRTPStreamConfiguration TLV8.
func EncodeSelectedStreamConfig(sessionID [16]byte, command byte, video VideoSelection, audio AudioSelection) []byte {
	sessionControl := TLV8Encode([]TLV8Item{
		{Type: TLVSessionControlID, Value: sessionID[:]},
		{Type: TLVSessionControlCommand, Value: []byte{command}},
	})

	bitrateBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(bitrateBytes, uint16(video.MaxBitrate))

	videoCodecParams := TLV8Encode([]TLV8Item{
		{Type: TLVVideoProfileID, Value: []byte{video.Profile}},
		{Type: TLVVideoLevel, Value: []byte{video.Level}},
		{Type: TLVVideoPacketizationMode, Value: []byte{0x00}}, // Non-interleaved
	})

	videoAttrs := TLV8Encode([]TLV8Item{
		{Type: 0x01, Value: uint16Bytes(video.Width)},
		{Type: 0x02, Value: uint16Bytes(video.Height)},
		{Type: 0x03, Value: []byte{byte(video.FPS)}},
	})

	ssrcBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(ssrcBytes, video.SSRC)

	videoRTP := TLV8Encode([]TLV8Item{
		{Type: TLVRTPPayloadType, Value: []byte{video.PayloadType}},
		{Type: TLVRTPSSRC, Value: ssrcBytes},
		{Type: TLVRTPMaxBitrate, Value: bitrateBytes},
		{Type: TLVRTPMinInterval, Value: float32Bytes(video.RTCPInterval)},
		{Type: TLVRTPMaxMTU, Value: uint16Bytes(1378)},
	})

	videoParams := TLV8Encode([]TLV8Item{
		{Type: TLVVideoCodecType, Value: []byte{VideoCodecH264}},
		{Type: TLVVideoCodecParams, Value: videoCodecParams},
		{Type: TLVVideoAttributes, Value: videoAttrs},
		{Type: TLVVideoRTPParams, Value: videoRTP},
	})

	audioCodecParams := TLV8Encode([]TLV8Item{
		{Type: TLVAudioChannels, Value: []byte{0x01}},
		{Type: TLVAudioBitRate, Value: []byte{audio.BitRateMode}},
		{Type: TLVAudioSampleRate, Value: []byte{audio.SampleRate}},
		{Type: TLVAudioPacketTime, Value: []byte{byte(audio.PacketTime)}},
	})

	audioSSRCBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(audioSSRCBytes, audio.SSRC)

	audioRTP := TLV8Encode([]TLV8Item{
		{Type: TLVRTPPayloadType, Value: []byte{audio.PayloadType}},
		{Type: TLVRTPSSRC, Value: audioSSRCBytes},
		{Type: TLVRTPMaxBitrate, Value: uint16Bytes(uint16(audio.MaxBitrate))},
		{Type: TLVRTPMinInterval, Value: float32Bytes(audio.RTCPInterval)},
		{Type: 0x06, Value: []byte{13}}, // ComfortNoisePayloadType
	})

	audioParams := TLV8Encode([]TLV8Item{
		{Type: TLVAudioCodecType, Value: []byte{audio.CodecType}},
		{Type: TLVAudioCodecParams, Value: audioCodecParams},
		{Type: TLVAudioRTPParams, Value: audioRTP},
		{Type: TLVAudioComfort, Value: []byte{0x00}}, // ComfortNoise disabled
	})

	return TLV8Encode([]TLV8Item{
		{Type: TLVSelectedSessionControl, Value: sessionControl},
		{Type: TLVSelectedVideoParams, Value: videoParams},
		{Type: TLVSelectedAudioParams, Value: audioParams},
	})
}

type VideoSelection struct {
	Profile      byte
	Level        byte
	Width        uint16
	Height       uint16
	FPS          int
	MaxBitrate   int
	SSRC         uint32
	PayloadType  byte
	RTCPInterval float32
}

type AudioSelection struct {
	CodecType    byte
	SampleRate   byte
	BitRateMode  byte // 0=Variable, 1=Constant
	PacketTime   int
	MaxBitrate   int
	SSRC         uint32
	PayloadType  byte
	RTCPInterval float32
}

func uint16Bytes(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

func float32Bytes(v float32) []byte {
	// RTP min interval is encoded as a IEEE 754 float32 in little-endian bytes.
	b := make([]byte, 4)
	bits := math.Float32bits(v)
	binary.LittleEndian.PutUint32(b, bits)
	return b
}
