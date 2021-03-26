package avp

import (
	"errors"
	"fmt"
	"net"
	"sync"

	log "github.com/pion/ion-log"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
)

const (
	maxSize = 100
)

var (
	// ErrCodecNotSupported is returned when a rtp packed it pushed with an unsupported codec
	ErrCodecNotSupported = errors.New("codec not supported")
)

type udpConn struct {
	conn        *net.UDPConn
	port        int
	payloadType uint8
}

// Builder Module for building video/audio samples from rtp streams
type Builder struct {
	mu            sync.RWMutex
	stopped       atomicBool
	onStopHandler func()
	builder       *samplebuilder.SampleBuilder
	elements      []Element
	sequence      uint16
	track         *webrtc.Track
	out           chan *Sample
}

// NewBuilder Initialize a new audio sample builder
func NewBuilder(track *webrtc.Track, maxLate uint16) *Builder {
	var depacketizer rtp.Depacketizer
	var checker rtp.PartitionHeadChecker
	switch track.Codec().Name {
	case webrtc.Opus:
		depacketizer = &codecs.OpusPacket{}
		checker = &codecs.OpusPartitionHeadChecker{}
	case webrtc.VP8:
		depacketizer = &codecs.VP8Packet{}
		checker = &codecs.VP8PartitionHeadChecker{}
	case webrtc.VP9:
		depacketizer = &codecs.VP9Packet{}
		checker = &codecs.VP9PartitionHeadChecker{}
	case webrtc.H264:
		depacketizer = &codecs.H264Packet{}
	}

	b := &Builder{
		builder: samplebuilder.New(maxLate, depacketizer),
		track:   track,
		out:     make(chan *Sample, maxSize),
	}

	if checker != nil {
		samplebuilder.WithPartitionHeadChecker(checker)(b.builder)
	}

	go b.build()
	go b.forward()

	return b
}

// AttachElement attaches a element to a builder
func (b *Builder) AttachElement(e Element) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.elements = append(b.elements, e)
}

// Track returns the builders underlying track
func (b *Builder) Track() *webrtc.Track {
	return b.track
}

// OnStop is called when a builder is stopped
func (b *Builder) OnStop(f func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onStopHandler = f
}

func (b *Builder) build() {
	log.Debugf("Reading rtp for track: %s", b.Track().ID())

	// Create a local addr
	var laddr *net.UDPAddr
	var err error
	if laddr, err = net.ResolveUDPAddr("udp", "127.0.0.1:"); err != nil {
		panic(err)
	}

	// Prepare udp conns
	// Also update incoming packets with expected PayloadType, the browser may use
	// a different value. We have to modify so our stream matches what rtp-forwarder.sdp expects
	udpConns := map[string]*udpConn{
		"audio": {port: 4000, payloadType: 111},
		"video": {port: 4002, payloadType: 96},
	}
	for _, c := range udpConns {
		// Create remote addr
		var raddr *net.UDPAddr
		if raddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", c.port)); err != nil {
			panic(err)
		}

		// Dial udp
		if c.conn, err = net.DialUDP("udp", laddr, raddr); err != nil {
			panic(err)
		}
		defer func(conn net.PacketConn) {
			if closeErr := conn.Close(); closeErr != nil {
				panic(closeErr)
			}
		}(c.conn)
	}

	// Retrieve udp connection
	c, ok := udpConns[b.track.Kind().String()]
	if !ok {
		return
	}

	buf := make([]byte, 1500)
	rtpPacket := &rtp.Packet{}

	for {
		if b.stopped.get() {
			return
		}

		n, readErr := b.track.Read(buf)
		if readErr != nil {
			panic(readErr)
		}

		// Unmarshal the packet and update the PayloadType
		if err = rtpPacket.Unmarshal(buf[:n]); err != nil {
			panic(err)
		}
		rtpPacket.PayloadType = c.payloadType

		// Marshal into original buffer with updated PayloadType
		if n, err = rtpPacket.MarshalTo(buf); err != nil {
			panic(err)
		}

		// Write
		if _, err = c.conn.Write(buf[:n]); err != nil {
			// For this particular example, third party applications usually timeout after a short
			// amount of time during which the user doesn't have enough time to provide the answer
			// to the browser.
			// That's why, for this particular example, the user first needs to provide the answer
			// to the browser then open the third party application. Therefore we must not kill
			// the forward on "connection refused" errors
			if opError, ok := err.(*net.OpError); ok && opError.Err.Error() == "write: connection refused" {
				continue
			}
			panic(err)
		}


		//pkt, err := b.track.ReadRTP()
		//if err != nil {
		//	if err == io.EOF {
		//		b.stop()
		//		return
		//	}
		//	log.Errorf("Error reading track rtp %s", err)
		//	continue
		//}
		//
		//if _, err = c.conn.Write(pkt.Raw); err != nil {
		//	if opError, ok := err.(*net.OpError); ok && opError.Err.Error() == "write: connection refused" {
		//		continue
		//	}
		//	panic(err)
		//}

		//b.builder.Push(pkt)
		//
		//for {
		//	log.Tracef("Read sample from builder: %s", b.Track().ID())
		//	sample, timestamp := b.builder.PopWithTimestamp()
		//	log.Tracef("Got sample from builder: %s sample: %v", b.Track().ID(), sample)
		//
		//	if b.stopped.get() {
		//		return
		//	}
		//
		//	if sample == nil {
		//		break
		//	}
		//
		//	b.out <- &Sample{
		//		ID:             b.track.ID(),
		//		Type:           int(b.track.Codec().Type),
		//		SequenceNumber: b.sequence,
		//		Timestamp:      timestamp,
		//		Payload:        sample.Data,
		//	}
		//	b.sequence++
		//}
	}
}

// Read sample
func (b *Builder) forward() {
	for {
		sample := <-b.out

		if b.stopped.get() {
			return
		}

		b.mu.RLock()
		for _, e := range b.elements {
			err := e.Write(sample)
			if err != nil {
				log.Errorf("error writing sample: %s", err)
			}
		}
		b.mu.RUnlock()
	}
}

// Stop stop all buffer
func (b *Builder) stop() {
	if b.stopped.get() {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.stopped.set(true)
	for _, e := range b.elements {
		e.Close()
	}
	if b.onStopHandler != nil {
		b.onStopHandler()
	}
	close(b.out)
}
