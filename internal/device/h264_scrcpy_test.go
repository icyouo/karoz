package device

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestH264DecoderConfigPrepareAndAVCC(t *testing.T) {
	cfg := &H264DecoderConfig{}
	if _, _, ok := cfg.AVCCConfig(); ok {
		t.Fatal("expected empty config before SPS/PPS")
	}

	sps := []byte{0x67, 0x42, 0xc0, 0x1e, 0xab}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	configPacket := append([]byte{0, 0, 0, 1}, sps...)
	configPacket = append(configPacket, 0, 0, 0, 1)
	configPacket = append(configPacket, pps...)

	out := cfg.Prepare(configPacket)
	if !bytes.Equal(out, configPacket) {
		t.Fatalf("config packet should pass through unchanged")
	}

	codec, avcc, ok := cfg.AVCCConfig()
	if !ok {
		t.Fatal("expected valid AVCC config after SPS+PPS")
	}
	if codec != "avc1.42c01e" {
		t.Fatalf("codec = %q, want avc1.42c01e", codec)
	}
	if len(avcc) != 11+len(sps)+len(pps) || avcc[0] != 1 || avcc[1] != 0x42 {
		t.Fatalf("unexpected avcC record: %x", avcc)
	}

	idr := []byte{0, 0, 0, 1, 0x65, 0x88, 0x84}
	prepared := cfg.Prepare(idr)
	nals := H264NALUnits(prepared)
	if len(nals) != 3 {
		t.Fatalf("prepared IDR NAL count = %d, want 3 (SPS, PPS, IDR)", len(nals))
	}
	if nals[0][0]&0x1f != 7 || nals[1][0]&0x1f != 8 || nals[2][0]&0x1f != 5 {
		t.Fatalf("unexpected NAL types: %v %v %v", nals[0][0]&0x1f, nals[1][0]&0x1f, nals[2][0]&0x1f)
	}
}

func TestScrcpyH264SourceAndTouchEncoding(t *testing.T) {
	var stream bytes.Buffer
	header := make([]byte, 77)
	copy(header[1:65], "Pixel_7")
	binary.BigEndian.PutUint32(header[65:69], ScrcpyCodecH264)
	binary.BigEndian.PutUint32(header[69:73], 1080)
	binary.BigEndian.PutUint32(header[73:77], 1920)
	stream.Write(header)

	writePacket := func(flagsAndPTS uint64, payload []byte) {
		var pktHdr [12]byte
		binary.BigEndian.PutUint64(pktHdr[:8], flagsAndPTS)
		binary.BigEndian.PutUint32(pktHdr[8:12], uint32(len(payload)))
		stream.Write(pktHdr[:])
		stream.Write(payload)
	}

	writePacket(ScrcpyConfigPacketFlag, []byte{0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1e})
	writePacket(2500, []byte{0, 0, 0, 1, 0x65, 0x01})

	src, err := NewScrcpyH264Source(io.NopCloser(&stream))
	if err != nil {
		t.Fatalf("NewScrcpyH264Source failed: %v", err)
	}
	if src.Header.Name != "Pixel_7" || src.Header.Width != 1080 || src.Header.Height != 1920 {
		t.Fatalf("unexpected scrcpy header: %+v", src.Header)
	}

	pkt1, err := src.ReadTimedPacket()
	if err != nil || !pkt1.Config || pkt1.PTS != 0 {
		t.Fatalf("pkt1 = %+v, err=%v", pkt1, err)
	}
	pkt2, err := src.ReadTimedPacket()
	if err != nil || pkt2.Config || pkt2.PTS != 2500*time.Microsecond {
		t.Fatalf("pkt2 = %+v, err=%v", pkt2, err)
	}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	injector := NewScrcpyTouchInjector("emulator-5554", c1)
	readDone := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 32)
		_, _ = io.ReadFull(c2, buf)
		readDone <- buf
	}()

	if err := injector.Inject(context.Background(), "down", 120, 340, 1080, 1920); err != nil {
		t.Fatalf("Inject failed: %v", err)
	}
	msg := <-readDone
	if len(msg) != 32 || msg[0] != ScrcpyControlInjectTouch || msg[1] != ScrcpyMotionActionDown {
		t.Fatalf("unexpected touch frame: %x", msg)
	}
	if gotX := binary.BigEndian.Uint32(msg[10:14]); gotX != 120 {
		t.Fatalf("touch x = %d, want 120", gotX)
	}
	if gotY := binary.BigEndian.Uint32(msg[14:18]); gotY != 340 {
		t.Fatalf("touch y = %d, want 340", gotY)
	}
}
