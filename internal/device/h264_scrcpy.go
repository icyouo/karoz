package device

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	ScrcpyCodecH264          uint32 = 0x68323634
	ScrcpyConfigPacketFlag   uint64 = 1 << 63
	ScrcpyPTSFlags           uint64 = 3 << 62
	ScrcpyControlInjectTouch byte   = 0x02
	ScrcpyMotionActionDown   byte   = 0
	ScrcpyMotionActionUp     byte   = 1
	ScrcpyMotionActionMove   byte   = 2
	ScrcpyTouchPressureMax   uint16 = 0xffff
)

// H264DecoderConfig caches SPS and PPS NAL units so fresh IDR keyframes and
// WebCodecs VideoDecoder sessions can be initialized at any point in a stream.
type H264DecoderConfig struct {
	sps []byte
	pps []byte
}

func (c *H264DecoderConfig) Prepare(packet []byte) []byte {
	hasSPS, hasPPS, hasIDR := false, false, false
	for _, nal := range H264NALUnits(packet) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case 7:
			hasSPS = true
			if !bytes.Equal(c.sps, nal) {
				c.sps = append([]byte(nil), nal...)
			}
		case 8:
			hasPPS = true
			if !bytes.Equal(c.pps, nal) {
				c.pps = append([]byte(nil), nal...)
			}
		case 5:
			hasIDR = true
		}
	}
	if !hasIDR || (hasSPS && hasPPS) || len(c.sps) == 0 || len(c.pps) == 0 {
		return packet
	}
	result := make([]byte, 0, len(c.sps)+len(c.pps)+len(packet)+12)
	result = append(result, 0, 0, 0, 1)
	result = append(result, c.sps...)
	result = append(result, 0, 0, 0, 1)
	result = append(result, c.pps...)
	if start, _ := H264StartCode(packet, 0); start != 0 {
		result = append(result, 0, 0, 0, 1)
	}
	return append(result, packet...)
}

// AVCCConfig emits the ISO/IEC 14496-15 AVCDecoderConfigurationRecord (`avcC`)
// and corresponding `avc1.PPCCLL` WebCodecs codec string once both SPS and PPS
// have been observed.
func (c *H264DecoderConfig) AVCCConfig() (codec string, avcc []byte, ok bool) {
	if c == nil || len(c.sps) < 4 || len(c.pps) == 0 {
		return "", nil, false
	}
	if len(c.sps) > 0xffff || len(c.pps) > 0xffff {
		return "", nil, false
	}
	codec = fmt.Sprintf("avc1.%02x%02x%02x", c.sps[1], c.sps[2], c.sps[3])
	avcc = make([]byte, 0, 11+len(c.sps)+len(c.pps))
	avcc = append(avcc,
		1,        // configurationVersion
		c.sps[1], // AVCProfileIndication
		c.sps[2], // profile_compatibility
		c.sps[3], // AVCLevelIndication
		0xff,     // 6 bits reserved (111111) + 2 bits lengthSizeMinusOne (3 => 4 bytes)
		0xe1,     // 3 bits reserved (111) + 5 bits numOfSequenceParameterSets (1)
		byte(len(c.sps)>>8), byte(len(c.sps)),
	)
	avcc = append(avcc, c.sps...)
	avcc = append(avcc, 1, byte(len(c.pps)>>8), byte(len(c.pps)))
	avcc = append(avcc, c.pps...)
	return codec, avcc, true
}

func H264NALUnits(packet []byte) [][]byte {
	start, width := H264StartCode(packet, 0)
	if start < 0 {
		return [][]byte{packet}
	}
	var nals [][]byte
	for start >= 0 {
		payload := start + width
		next, nextWidth := H264StartCode(packet, payload)
		if next < 0 {
			nals = append(nals, packet[payload:])
			break
		}
		nals = append(nals, packet[payload:next])
		start, width = next, nextWidth
	}
	return nals
}

func H264StartCode(packet []byte, offset int) (int, int) {
	for i := offset; i+3 <= len(packet); i++ {
		if packet[i] != 0 || packet[i+1] != 0 {
			continue
		}
		if packet[i+2] == 1 {
			return i, 3
		}
		if i+4 <= len(packet) && packet[i+2] == 0 && packet[i+3] == 1 {
			return i, 4
		}
	}
	return -1, 0
}

type ScrcpyStreamHeader struct {
	Name   string
	Codec  uint32
	Width  int
	Height int
}

func ReadScrcpyStreamHeader(r io.Reader) (ScrcpyStreamHeader, error) {
	header := make([]byte, 1+64+4+4+4)
	if _, err := io.ReadFull(r, header); err != nil {
		return ScrcpyStreamHeader{}, fmt.Errorf("read scrcpy stream header: %w", err)
	}
	codec := binary.BigEndian.Uint32(header[65:69])
	if codec != ScrcpyCodecH264 {
		return ScrcpyStreamHeader{}, fmt.Errorf("scrcpy video codec is 0x%x, want h264", codec)
	}
	return ScrcpyStreamHeader{
		Name:   strings.TrimRight(string(header[1:65]), "\x00"),
		Codec:  codec,
		Width:  int(binary.BigEndian.Uint32(header[69:73])),
		Height: int(binary.BigEndian.Uint32(header[73:77])),
	}, nil
}

type TimedH264Packet struct {
	Data   []byte
	PTS    time.Duration
	Config bool
}

type ScrcpyH264Source struct {
	Header ScrcpyStreamHeader
	conn   io.ReadCloser
	buffer []byte
	once   sync.Once
	err    error
}

func NewScrcpyH264Source(conn io.ReadCloser) (*ScrcpyH264Source, error) {
	header, err := ReadScrcpyStreamHeader(conn)
	if err != nil {
		return nil, err
	}
	return &ScrcpyH264Source{Header: header, conn: conn}, nil
}

func (s *ScrcpyH264Source) Read(p []byte) (int, error) {
	for len(s.buffer) == 0 {
		packet, err := s.ReadPacket()
		if err != nil {
			return 0, err
		}
		s.buffer = packet
	}
	n := copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	return n, nil
}

func (s *ScrcpyH264Source) ReadPacket() ([]byte, error) {
	packet, err := s.ReadTimedPacket()
	return packet.Data, err
}

func (s *ScrcpyH264Source) ReadTimedPacket() (TimedH264Packet, error) {
	for {
		var header [12]byte
		if _, err := io.ReadFull(s.conn, header[:]); err != nil {
			return TimedH264Packet{}, err
		}
		packetLen := binary.BigEndian.Uint32(header[8:12])
		if packetLen == 0 {
			continue
		}
		if packetLen > 16*1024*1024 {
			return TimedH264Packet{}, fmt.Errorf("scrcpy packet is too large: %d", packetLen)
		}
		packet := make([]byte, packetLen)
		if _, err := io.ReadFull(s.conn, packet); err != nil {
			return TimedH264Packet{}, err
		}
		ptsFlags := binary.BigEndian.Uint64(header[:8])
		config := ptsFlags&ScrcpyConfigPacketFlag != 0
		var pts time.Duration
		if !config {
			ptsMicros := ptsFlags &^ ScrcpyPTSFlags
			if ptsMicros > uint64((1<<63-1)/int64(time.Microsecond)) {
				return TimedH264Packet{}, fmt.Errorf("scrcpy timestamp is too large: %d", ptsMicros)
			}
			pts = time.Duration(ptsMicros) * time.Microsecond
		}
		return TimedH264Packet{Data: packet, PTS: pts, Config: config}, nil
	}
}

func (s *ScrcpyH264Source) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	s.once.Do(func() {
		s.err = s.conn.Close()
	})
	return s.err
}

type ScrcpyTouchInjector struct {
	target string
	conn   net.Conn
	mu     sync.Mutex
	closed bool
}

func NewScrcpyTouchInjector(target string, conn net.Conn) *ScrcpyTouchInjector {
	return &ScrcpyTouchInjector{target: strings.TrimSpace(target), conn: conn}
}

func (i *ScrcpyTouchInjector) Inject(ctx context.Context, phase string, x, y, viewportWidth, viewportHeight int) error {
	if i == nil || i.conn == nil {
		return fmt.Errorf("scrcpy control is not connected")
	}
	action, err := ScrcpyActionForTouchPhase(phase)
	if err != nil {
		return err
	}
	if viewportWidth <= 0 {
		viewportWidth = maxInt(x+1, 1)
	}
	if viewportHeight <= 0 {
		viewportHeight = maxInt(y+1, 1)
	}
	msg := EncodeScrcpyTouchEvent(action, 0, x, y, viewportWidth, viewportHeight)

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return fmt.Errorf("scrcpy control is closed")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = i.conn.SetWriteDeadline(deadline)
	} else {
		_ = i.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
	}
	_, err = i.conn.Write(msg)
	_ = i.conn.SetWriteDeadline(time.Time{})
	return err
}

func (i *ScrcpyTouchInjector) Close() error {
	if i == nil || i.conn == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	return i.conn.Close()
}

func ScrcpyActionForTouchPhase(phase string) (byte, error) {
	switch strings.TrimSpace(phase) {
	case "down":
		return ScrcpyMotionActionDown, nil
	case "move":
		return ScrcpyMotionActionMove, nil
	case "up":
		return ScrcpyMotionActionUp, nil
	default:
		return 0, fmt.Errorf("unsupported touch phase %q", phase)
	}
}

func EncodeScrcpyTouchEvent(action byte, pointerID int64, x, y, screenWidth, screenHeight int) []byte {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if screenWidth <= 0 {
		screenWidth = 1
	}
	if screenHeight <= 0 {
		screenHeight = 1
	}
	if x >= screenWidth {
		x = screenWidth - 1
	}
	if y >= screenHeight {
		y = screenHeight - 1
	}
	msg := make([]byte, 32)
	msg[0] = ScrcpyControlInjectTouch
	msg[1] = action
	binary.BigEndian.PutUint64(msg[2:10], uint64(pointerID))
	binary.BigEndian.PutUint32(msg[10:14], uint32(x))
	binary.BigEndian.PutUint32(msg[14:18], uint32(y))
	binary.BigEndian.PutUint16(msg[18:20], uint16Clamp(screenWidth))
	binary.BigEndian.PutUint16(msg[20:22], uint16Clamp(screenHeight))
	binary.BigEndian.PutUint16(msg[22:24], ScrcpyTouchPressureMax)
	binary.BigEndian.PutUint32(msg[24:28], 0)
	binary.BigEndian.PutUint32(msg[28:32], 0)
	return msg
}

func uint16Clamp(value int) uint16 {
	if value < 0 {
		return 0
	}
	if value > 0xffff {
		return 0xffff
	}
	return uint16(value)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
