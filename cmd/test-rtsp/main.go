package main

import (
	"fmt"
	"log"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// Minimal H.264 SPS/PPS for 320x240 Baseline.
var testSPS = []byte{0x67, 0x42, 0xc0, 0x0a, 0xd9, 0x07, 0x3c, 0x04, 0x40}
var testPPS = []byte{0x68, 0xce, 0x38, 0x80}

type handler struct {
	server *gortsplib.Server
	stream *gortsplib.ServerStream
	desc   *description.Session
	media  *description.Media
	format *format.H264
}

func (h *handler) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	log.Println("conn opened")
}

func (h *handler) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	log.Println("conn closed")
}

func (h *handler) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	log.Println("session opened")
}

func (h *handler) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	log.Println("session closed")
}

func (h *handler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	log.Println("DESCRIBE")
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *handler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	log.Println("SETUP")
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *handler) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	log.Println("PLAY")

	go func() {
		// Wait for writer to be ready.
		time.Sleep(200 * time.Millisecond)

		seq := uint16(0)
		ts := uint32(0)

		// Send STAP-A with SPS+PPS.
		stapA := buildSTAPA(testSPS, testPPS)
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: seq,
				Timestamp:      ts,
				Marker:         false,
			},
			Payload: stapA,
		}
		log.Printf("writing STAP-A seq=%d ts=%d size=%d", seq, ts, len(stapA))
		h.stream.WritePacketRTP(h.media, pkt)
		seq++

		// Send a fake IDR frame (just a small one).
		idr := make([]byte, 100)
		idr[0] = 0x65 // IDR NALU type 5, NRI=3
		for i := 1; i < len(idr); i++ {
			idr[i] = byte(i)
		}
		pkt = &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: seq,
				Timestamp:      ts,
				Marker:         true, // end of access unit
			},
			Payload: idr,
		}
		log.Printf("writing IDR seq=%d ts=%d size=%d marker=true", seq, ts, len(idr))
		h.stream.WritePacketRTP(h.media, pkt)
		seq++

		// Send P-frames every 50ms (20fps).
		for i := 0; i < 200; i++ {
			ts += 4500 // 90000/20 = 4500
			pframe := make([]byte, 500)
			pframe[0] = 0x41 // non-IDR slice, NRI=2, type=1
			for j := 1; j < len(pframe); j++ {
				pframe[j] = byte(j + i)
			}
			pkt = &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: seq,
					Timestamp:      ts,
					Marker:         true,
				},
				Payload: pframe,
			}
			if i < 3 || i%50 == 0 {
				log.Printf("writing P-frame seq=%d ts=%d size=%d", seq, ts, len(pframe))
			}
			h.stream.WritePacketRTP(h.media, pkt)
			seq++
			time.Sleep(50 * time.Millisecond)
		}
		log.Println("done sending")
	}()

	return &base.Response{StatusCode: base.StatusOK}, nil
}

func buildSTAPA(sps, pps []byte) []byte {
	// STAP-A: [header] [2-byte size] [NALU] [2-byte size] [NALU]
	buf := make([]byte, 0, 1+2+len(sps)+2+len(pps))
	buf = append(buf, 24) // STAP-A type
	buf = append(buf, byte(len(sps)>>8), byte(len(sps)))
	buf = append(buf, sps...)
	buf = append(buf, byte(len(pps)>>8), byte(len(pps)))
	buf = append(buf, pps...)
	return buf
}

func main() {
	h := &handler{}

	h.format = &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
		SPS:               testSPS,
		PPS:               testPPS,
	}

	h.media = &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{h.format},
	}

	h.desc = &description.Session{
		Medias: []*description.Media{h.media},
	}

	h.server = &gortsplib.Server{
		Handler:     h,
		RTSPAddress: ":8555",
	}

	if err := h.server.Start(); err != nil {
		log.Fatal(err)
	}

	h.stream = &gortsplib.ServerStream{
		Server: h.server,
		Desc:   h.desc,
	}
	if err := h.stream.Initialize(); err != nil {
		log.Fatal(err)
	}

	fmt.Println("test RTSP server listening on rtsp://localhost:8555/test")
	select {}
}
