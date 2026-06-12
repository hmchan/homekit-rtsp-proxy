package stream

/*
#cgo LDFLAGS: -lfdk-aac
#include <fdk-aac/aacdecoder_lib.h>
#include <fdk-aac/aacenc_lib.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

// Configure decoder with raw AudioSpecificConfig.
// Wraps aacDecoder_ConfigRaw to avoid Go pointer issues with UCHAR**.
static AAC_DECODER_ERROR config_raw(HANDLE_AACDECODER dec, unsigned char *conf, unsigned int confLen) {
    UCHAR *confArray[1] = { conf };
    UINT   lenArray[1]  = { confLen };
    return aacDecoder_ConfigRaw(dec, confArray, lenArray);
}

// Get decoder stream info as a string for diagnostics.
static void get_stream_info(HANDLE_AACDECODER dec, char *buf, int bufLen) {
    CStreamInfo *info = aacDecoder_GetStreamInfo(dec);
    if (info == NULL) {
        snprintf(buf, bufLen, "null");
        return;
    }
    snprintf(buf, bufLen, "aot=%d sampleRate=%d frameSize=%d numChannels=%d flags=0x%x",
             info->aot, info->sampleRate, info->frameSize, info->numChannels, info->flags);
}

// Decode one AAC frame to PCM.
// Returns number of decoded samples on success, negative on error.
static int decode_frame(HANDLE_AACDECODER dec, unsigned char *in, int inLen, INT_PCM *out, int outLen) {
    UINT bytesValid = (UINT)inLen;
    UCHAR *inBuf = in;
    UINT bufSize = (UINT)inLen;

    AAC_DECODER_ERROR err = aacDecoder_Fill(dec, &inBuf, &bufSize, &bytesValid);
    if (err != AAC_DEC_OK) {
        return -1;
    }

    err = aacDecoder_DecodeFrame(dec, out, outLen, 0);
    if (err != AAC_DEC_OK) {
        return -2;
    }

    CStreamInfo *info = aacDecoder_GetStreamInfo(dec);
    if (info == NULL) {
        return -3;
    }
    return info->frameSize * info->numChannels;
}

// Dump PCM samples directly from C to a file, bypassing Go memory.
static void dump_pcm_c(INT_PCM *buf, int nSamples, const char *path) {
    FILE *f = fopen(path, "ab");
    if (!f) return;
    fwrite(buf, sizeof(INT_PCM), nSamples, f);
    fclose(f);
}

// Print first 8 PCM samples for diagnostics.
static void print_pcm_samples(INT_PCM *buf, int nSamples, int frameNum) {
    int n = nSamples < 8 ? nSamples : 8;
    printf("[pcm_raw] frame=%d nSamples=%d sizeof(INT_PCM)=%d first8:", frameNum, nSamples, (int)sizeof(INT_PCM));
    for (int i = 0; i < n; i++) {
        printf(" %d", (int)buf[i]);
    }
    printf("\n");
}

// Encode one PCM frame to AAC-LC.
// Returns encoded size on success, negative on error.
static int encode_frame(HANDLE_AACENCODER enc, INT_PCM *in, int inSamples, unsigned char *out, int outLen) {
    AACENC_BufDesc inBufDesc  = {0};
    AACENC_BufDesc outBufDesc = {0};
    AACENC_InArgs  inArgs     = {0};
    AACENC_OutArgs outArgs    = {0};

    // Input buffer setup
    int inBufId       = IN_AUDIO_DATA;
    int inBufSize     = inSamples * (int)sizeof(INT_PCM);
    int inBufElSize   = (int)sizeof(INT_PCM);
    void *inPtr       = (void *)in;

    inBufDesc.numBufs           = 1;
    inBufDesc.bufs              = &inPtr;
    inBufDesc.bufferIdentifiers = &inBufId;
    inBufDesc.bufSizes          = &inBufSize;
    inBufDesc.bufElSizes        = &inBufElSize;

    // Output buffer setup
    int outBufId      = OUT_BITSTREAM_DATA;
    int outBufSize    = outLen;
    int outBufElSize  = 1;
    void *outPtr      = (void *)out;

    outBufDesc.numBufs           = 1;
    outBufDesc.bufs              = &outPtr;
    outBufDesc.bufferIdentifiers = &outBufId;
    outBufDesc.bufSizes          = &outBufSize;
    outBufDesc.bufElSizes        = &outBufElSize;

    inArgs.numInSamples = inSamples;

    AACENC_ERROR err = aacEncEncode(enc, &inBufDesc, &outBufDesc, &inArgs, &outArgs);
    if (err != AACENC_OK) {
        return -1;
    }
    return outArgs.numOutBytes;
}
*/
import "C"

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"unsafe"
)

// AudioTranscoder transcodes AAC-ELD audio to AAC-LC using libfdk-aac.
// AAC-ELD uses 480 or 512 samples/frame; AAC-LC uses 1024. We accumulate
// decoded PCM in a ring buffer and encode whenever we have a full LC frame.
type AudioTranscoder struct {
	logger *slog.Logger
	gain int // PCM gain factor (0 = mute, 512 = ~54dB)
	decoder C.HANDLE_AACDECODER
	encoder C.HANDLE_AACENCODER
	outASC  []byte  // AAC-LC AudioSpecificConfig from encoder
	decBuf  []C.INT_PCM // temp buffer for one decoded ELD frame
	pcmRing []C.INT_PCM // accumulator for PCM samples
	pcmLen  int         // current number of valid samples in pcmRing
	encBuf  []byte      // output buffer for encoder
	encFrameSize int    // encoder's frame size (1024 for AAC-LC)
	diagCount    int       // diagnostic counter
	frameCount   int       // total frames decoded by production decoder

	// Auto-detection: try ASC candidates on first frames to find the right one.
	ascCandidates []string
	sampleRate    int
	eldASCHex     string // the ASC we're currently using
	detecting     bool   // true during auto-detection phase
	detectFrames  [][]byte // buffered raw frames for detection
}

// eldASCCandidates returns possible AudioSpecificConfig hex strings to try
// for AAC-ELD at the given sample rate. The camera may use plain ELD or
// ELD with LD-SBR (core at half rate), and 480 or 512 sample frames.
func eldASCCandidates(sampleRate int) []string {
	// ASC bit layout for ELD (AOT 39):
	//   bits 0-4:  11111 (escape)
	//   bits 5-10: 000111 (ext type 7 → AOT 39)
	//   bits 11-14: frequency index (8=16kHz, 11=8kHz)
	//   bits 15-18: channel config (1=mono)
	//   bit 19: frameLengthFlag (0=512, 1=480)
	//   bits 20-22: resilience flags (000)
	//   bit 23: ldSbrPresentFlag
	//
	// For 16kHz output:
	//   F8F02000 = ELD 16kHz(idx=8)  mono, 512, no SBR
	//   F8F03000 = ELD 16kHz(idx=8)  mono, 480, no SBR
	//   F8F62100 = ELD 8kHz(idx=11)  mono, 512, LD-SBR → 16kHz output
	//   F8F63100 = ELD 8kHz(idx=11)  mono, 480, LD-SBR → 16kHz output
	switch sampleRate {
	case 16000:
		return []string{
			"F8F62100", // 8kHz core + LD-SBR → 16kHz, 512 samples
			"F8F63100", // 8kHz core + LD-SBR → 16kHz, 480 samples
			"F8F02000", // 16kHz, 512 samples, no SBR
			"F8F03000", // 16kHz, 480 samples, no SBR
		}
	case 24000:
		return []string{
			"F8F22100", // 12kHz(idx=9) core + LD-SBR → 24kHz
			"F8EC2000", // 24kHz, no SBR
		}
	case 8000:
		return []string{
			"F8F82000", // 8kHz(idx=11), 512, no SBR
			"F8F83000", // 8kHz(idx=11), 480, no SBR
		}
	default:
		return []string{"F8F02000"}
	}
}

// NewAudioTranscoder creates a transcoder that decodes AAC-ELD and encodes AAC-LC.
func NewAudioTranscoder(sampleRate int, eldASCHex string, gain int, logger *slog.Logger) (*AudioTranscoder, error) {
	// Plain ELD without SBR matches this camera's output. Auto-detection
	// (still available via ascCandidates) found SBR ASCs decoded ~60dB
	// too quiet.
	asc := eldASCHex // default from caller

	t, err := newTranscoderWithASC(sampleRate, asc)
	if err != nil {
		return nil, fmt.Errorf("init transcoder with ASC %s: %w", asc, err)
	}

	t.logger = logger
	t.gain = gain
	t.sampleRate = sampleRate
	t.detecting = false // no auto-detection

	logger.Info("audio transcoder configured", "asc", asc, "sampleRate", sampleRate, "gain", gain)
	return t, nil
}

func newTranscoderWithASC(sampleRate int, eldASCHex string) (*AudioTranscoder, error) {
	t := &AudioTranscoder{eldASCHex: eldASCHex}

	// --- Decoder (AAC-ELD → PCM) ---
	t.decoder = C.aacDecoder_Open(C.TT_MP4_RAW, 1)
	if t.decoder == nil {
		return nil, fmt.Errorf("aacDecoder_Open failed")
	}

	// Feed AudioSpecificConfig to decoder.
	ascBytes, err := hex.DecodeString(strings.TrimSpace(eldASCHex))
	if err != nil {
		C.aacDecoder_Close(t.decoder)
		return nil, fmt.Errorf("invalid ASC hex %q: %w", eldASCHex, err)
	}

	decErr := C.config_raw(t.decoder, (*C.uchar)(unsafe.Pointer(&ascBytes[0])), C.uint(len(ascBytes)))
	if decErr != C.AAC_DEC_OK {
		C.aacDecoder_Close(t.decoder)
		return nil, fmt.Errorf("aacDecoder_ConfigRaw failed: %d", decErr)
	}

	// Disable DRC and PCM limiter — camera's bitstream may contain DRC metadata
	// that causes heavy attenuation of the decoded output.
	C.aacDecoder_SetParam(t.decoder, C.AAC_PCM_LIMITER_ENABLE, 0)
	C.aacDecoder_SetParam(t.decoder, C.AAC_DRC_REFERENCE_LEVEL, C.INT(-1))
	C.aacDecoder_SetParam(t.decoder, C.AAC_DRC_ATTENUATION_FACTOR, 0)
	C.aacDecoder_SetParam(t.decoder, C.AAC_DRC_BOOST_FACTOR, 0)

	// --- Encoder (PCM → AAC-LC) ---
	var encoder C.HANDLE_AACENCODER
	encErr := C.aacEncOpen(&encoder, 0, 1) // 1 channel
	if encErr != C.AACENC_OK {
		C.aacDecoder_Close(t.decoder)
		return nil, fmt.Errorf("aacEncOpen failed: %d", encErr)
	}
	t.encoder = encoder

	// Configure encoder.
	params := []struct {
		param C.AACENC_PARAM
		value int
	}{
		{C.AACENC_AOT, C.AOT_AAC_LC},
		{C.AACENC_SAMPLERATE, sampleRate},
		{C.AACENC_CHANNELMODE, C.MODE_1}, // mono
		{C.AACENC_BITRATE, 64000},
		{C.AACENC_TRANSMUX, C.TT_MP4_RAW}, // raw frames (we add AU headers ourselves)
	}
	for _, p := range params {
		if e := C.aacEncoder_SetParam(t.encoder, p.param, C.UINT(p.value)); e != C.AACENC_OK {
			t.Close()
			return nil, fmt.Errorf("aacEncoder_SetParam(%d) failed: %d", p.param, e)
		}
	}

	if e := C.aacEncEncode(t.encoder, nil, nil, nil, nil); e != C.AACENC_OK {
		t.Close()
		return nil, fmt.Errorf("aacEncEncode init failed: %d", e)
	}

	// Get encoder info (frame size, ASC).
	var info C.AACENC_InfoStruct
	if e := C.aacEncInfo(t.encoder, &info); e != C.AACENC_OK {
		t.Close()
		return nil, fmt.Errorf("aacEncInfo failed: %d", e)
	}

	t.encFrameSize = int(info.frameLength)

	// Extract ASC from encoder info.
	ascSize := int(info.confSize)
	t.outASC = make([]byte, ascSize)
	for i := 0; i < ascSize; i++ {
		t.outASC[i] = byte(info.confBuf[i])
	}

	// Allocate buffers.
	t.decBuf = make([]C.INT_PCM, 8192)  // one decoded ELD frame (480-512 samples, room to spare)
	t.pcmRing = make([]C.INT_PCM, t.encFrameSize*3) // accumulator (holds ~3 LC frames worth)
	t.encBuf = make([]byte, 2048)

	return t, nil
}

const detectFrameCount = 20 // number of frames to buffer for ASC auto-detection

// Transcode decodes one raw AAC-ELD frame, accumulates PCM, and encodes
// AAC-LC frames when enough samples are available. Returns nil output (no
// error) when more input is needed before a full LC frame can be produced.
func (t *AudioTranscoder) Transcode(aacELDFrame []byte) ([]byte, error) {
	if len(aacELDFrame) == 0 {
		return nil, fmt.Errorf("empty input frame")
	}

	// Auto-detection phase: buffer frames, then try each ASC candidate.
	if t.detecting {
		frameCopy := make([]byte, len(aacELDFrame))
		copy(frameCopy, aacELDFrame)
		t.detectFrames = append(t.detectFrames, frameCopy)

		if len(t.detectFrames) < detectFrameCount {
			return nil, nil // keep buffering
		}

		// We have enough frames. Try each ASC and pick the best.
		t.detecting = false
		bestASC, err := t.autoDetectASC()
		if err != nil {
			t.logger.Warn("audio ASC auto-detect failed", "error", err, "keeping", t.eldASCHex)
		} else if bestASC != t.eldASCHex {
			t.logger.Info("audio ASC switched", "from", t.eldASCHex, "to", bestASC)
			// Re-create decoder with the winning ASC.
			C.aacDecoder_Close(t.decoder)
			t.decoder = C.aacDecoder_Open(C.TT_MP4_RAW, 1)
			ascBytes, _ := hex.DecodeString(bestASC)
			C.config_raw(t.decoder, (*C.uchar)(unsafe.Pointer(&ascBytes[0])), C.uint(len(ascBytes)))
			t.eldASCHex = bestASC
		}

		// Now decode all buffered frames through the (possibly new) decoder.
		for _, frame := range t.detectFrames {
			// Ignore output during replay — just priming the decoder.
			t.transcodeFrame(frame)
		}
		t.detectFrames = nil
		return nil, nil
	}

	return t.transcodeFrame(aacELDFrame)
}

// autoDetectASC tries each ASC candidate on the buffered frames and returns
// the one that produces the highest peak PCM (i.e., real audio, not silence).
func (t *AudioTranscoder) autoDetectASC() (string, error) {
	type result struct {
		asc     string
		maxPCM  int
		decoded int
		err     error
	}

	var results []result

	for _, asc := range t.ascCandidates {
		// Create a temporary decoder with this ASC.
		dec := C.aacDecoder_Open(C.TT_MP4_RAW, 1)
		if dec == nil {
			results = append(results, result{asc: asc, err: fmt.Errorf("open failed")})
			continue
		}

		ascBytes, err := hex.DecodeString(asc)
		if err != nil {
			C.aacDecoder_Close(dec)
			results = append(results, result{asc: asc, err: err})
			continue
		}

		rc := C.config_raw(dec, (*C.uchar)(unsafe.Pointer(&ascBytes[0])), C.uint(len(ascBytes)))
		if rc != C.AAC_DEC_OK {
			C.aacDecoder_Close(dec)
			results = append(results, result{asc: asc, err: fmt.Errorf("config failed: %d", rc)})
			continue
		}

		// Decode all buffered frames and measure peak PCM.
		tmpBuf := make([]C.INT_PCM, 8192)
		var maxPCM C.INT_PCM
		decodedFrames := 0

		for _, frame := range t.detectFrames {
			ns := C.decode_frame(
				dec,
				(*C.uchar)(unsafe.Pointer(&frame[0])),
				C.int(len(frame)),
				&tmpBuf[0],
				C.int(len(tmpBuf)),
			)
			if ns <= 0 {
				continue
			}
			decodedFrames++
			for i := 0; i < int(ns); i++ {
				s := tmpBuf[i]
				if s < 0 {
					s = -s
				}
				if s > maxPCM {
					maxPCM = s
				}
			}
		}

		// Get stream info for diagnostics.
		var infoBuf [256]C.char
		C.get_stream_info(dec, &infoBuf[0], 256)
		streamInfo := C.GoString(&infoBuf[0])

		C.aacDecoder_Close(dec)
		results = append(results, result{
			asc:     asc,
			maxPCM:  int(maxPCM),
			decoded: decodedFrames,
		})
		t.logger.Debug("audio ASC candidate tested",
			"asc", asc, "decoded", decodedFrames, "of", len(t.detectFrames),
			"maxPCM", int(maxPCM), "info", streamInfo)
	}

	// Pick the ASC with the highest peak PCM.
	bestIdx := -1
	bestPCM := 0
	for i, r := range results {
		if r.err == nil && r.maxPCM > bestPCM {
			bestPCM = r.maxPCM
			bestIdx = i
		}
	}

	if bestIdx < 0 {
		return "", fmt.Errorf("no valid ASC candidate found")
	}

	t.logger.Info("audio ASC auto-detected", "asc", results[bestIdx].asc, "maxPCM", bestPCM)
	return results[bestIdx].asc, nil
}

// transcodeFrame decodes one raw AAC-ELD frame and encodes to AAC-LC.
func (t *AudioTranscoder) transcodeFrame(aacELDFrame []byte) ([]byte, error) {
	// Decode AAC-ELD → PCM.
	nSamples := C.decode_frame(
		t.decoder,
		(*C.uchar)(unsafe.Pointer(&aacELDFrame[0])),
		C.int(len(aacELDFrame)),
		&t.decBuf[0],
		C.int(len(t.decBuf)),
	)
	if nSamples < 0 {
		return nil, fmt.Errorf("AAC-ELD decode failed: %d", nSamples)
	}

	decoded := int(nSamples)
	t.frameCount++

	// Peak-PCM diagnostics, debug level only (was the main source of log
	// spam: an unconditional printf once per second, forever).
	if t.logger.Enabled(nil, slog.LevelDebug) && (t.frameCount <= 5 || t.frameCount%50 == 0) {
		var maxPCM C.INT_PCM
		for i := 0; i < decoded; i++ {
			s := t.decBuf[i]
			if s < 0 {
				s = -s
			}
			if s > maxPCM {
				maxPCM = s
			}
		}
		t.logger.Debug("audio transcode stats",
			"frame", t.frameCount, "decoded", decoded, "maxPCM", int(maxPCM))
	}


	if t.pcmLen+decoded > len(t.pcmRing) {
		return nil, fmt.Errorf("PCM ring overflow: %d + %d > %d", t.pcmLen, decoded, len(t.pcmRing))
	}

	// Amplify decoded PCM. The camera's AAC-ELD decoder output is ~60 dB too
	// quiet (maxPCM ~7 out of 32767). Apply gain to bring to normal levels.
	// Clamp to int16 range to prevent clipping. Gain of 0 = passthrough.
	if t.gain != 0 {
		for i := 0; i < decoded; i++ {
			v := int32(t.decBuf[i]) * int32(t.gain)
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			t.decBuf[i] = C.INT_PCM(v)
		}
	}

	copy(t.pcmRing[t.pcmLen:], t.decBuf[:decoded])
	t.pcmLen += decoded

	// Not enough samples for an encoder frame yet.
	if t.pcmLen < t.encFrameSize {
		return nil, nil
	}

	// Encode one AAC-LC frame (1024 samples).
	n := C.encode_frame(
		t.encoder,
		&t.pcmRing[0],
		C.int(t.encFrameSize),
		(*C.uchar)(unsafe.Pointer(&t.encBuf[0])),
		C.int(len(t.encBuf)),
	)
	if n < 0 {
		return nil, fmt.Errorf("AAC-LC encode failed: %d", n)
	}

	// Shift remaining samples to front of ring.
	remaining := t.pcmLen - t.encFrameSize
	if remaining > 0 {
		copy(t.pcmRing[:remaining], t.pcmRing[t.encFrameSize:t.pcmLen])
	}
	t.pcmLen = remaining

	if n == 0 {
		return nil, nil
	}

	out := make([]byte, int(n))
	copy(out, t.encBuf[:int(n)])
	return out, nil
}

// TestDecodePeakPCM creates a temporary decoder to test-decode data and returns
// the peak absolute PCM sample value. Used for diagnostics to determine if
// AU headers should be stripped or not.
func (t *AudioTranscoder) TestDecodePeakPCM(data []byte) int {
	if len(data) == 0 {
		return -1
	}

	// Create a throwaway decoder with same ASC.
	dec := C.aacDecoder_Open(C.TT_MP4_RAW, 1)
	if dec == nil {
		return -2
	}
	defer C.aacDecoder_Close(dec)

	ascBytes, err := hex.DecodeString(t.eldASCHex)
	if err != nil {
		return -3
	}

	rc := C.config_raw(dec, (*C.uchar)(unsafe.Pointer(&ascBytes[0])), C.uint(len(ascBytes)))
	if rc != C.AAC_DEC_OK {
		return -4
	}

	tmpBuf := make([]C.INT_PCM, 8192)
	ns := C.decode_frame(dec, (*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data)), &tmpBuf[0], C.int(len(tmpBuf)))
	if ns <= 0 {
		return int(ns) // negative = decode error
	}

	var maxPCM C.INT_PCM
	for i := 0; i < int(ns); i++ {
		s := tmpBuf[i]
		if s < 0 {
			s = -s
		}
		if s > maxPCM {
			maxPCM = s
		}
	}
	return int(maxPCM)
}

// AudioSpecificConfig returns the AAC-LC AudioSpecificConfig from the encoder,
// suitable for use in SDP config= parameter.
func (t *AudioTranscoder) AudioSpecificConfig() []byte {
	return t.outASC
}

// AudioSpecificConfigHex returns the ASC as a hex string for SDP.
func (t *AudioTranscoder) AudioSpecificConfigHex() string {
	return strings.ToUpper(hex.EncodeToString(t.outASC))
}

// Close releases all FDK-AAC resources.
func (t *AudioTranscoder) Close() {
	if t.decoder != nil {
		C.aacDecoder_Close(t.decoder)
		t.decoder = nil
	}
	if t.encoder != nil {
		C.aacEncClose(&t.encoder)
		t.encoder = nil
	}
}
