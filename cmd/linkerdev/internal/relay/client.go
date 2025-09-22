package relay

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// relayClient handles communication with the relay
type relayClient struct {
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	writeMu sync.Mutex
	streams sync.Map // streamID -> net.Conn (local app connection)
}

// Protocol constants
const (
	tOpen            = 0x01
	tData            = 0x02
	tClose           = 0x03
	tRst             = 0x04
	tOutboundConnect = 0x05
	tPing            = 0x10
	tPong            = 0x11
)

// frame represents a protocol frame
type frame struct {
	typ      byte
	streamID uint32
	payload  []byte
}

// NewRelayClient creates a new relay client connection
func NewRelayClient(addr string) (*relayClient, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	rc := &relayClient{
		conn: c,
		br:   bufio.NewReader(c),
		bw:   bufio.NewWriterSize(c, 64<<10),
	}
	_ = rc.sendFrame(tPing, 0, []byte("HELLO LINKERDEV"))
	return rc, nil
}

// Loop handles incoming frames from the relay
func (rc *relayClient) Loop(localAppPort int) {
	defer rc.conn.Close()
	for {
		fr, err := rc.readFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("[relay] control read error: %v", err)
			}
			return
		}
		rc.handleFrame(fr, localAppPort)
	}
}

// SendOutboundConnect sends an outbound connect request to the relay
func (rc *relayClient) SendOutboundConnect(destination string) error {
	// Generate a unique stream ID for this outbound connection
	streamID := uint32(time.Now().UnixNano() & 0xFFFFFFFF)

	// Send the outbound connect frame
	return rc.sendFrame(tOutboundConnect, streamID, []byte(destination))
}

// readFrame reads a frame from the reader
func (rc *relayClient) readFrame() (*frame, error) {
	var hdr [9]byte
	if _, err := io.ReadFull(rc.br, hdr[:]); err != nil {
		return nil, err
	}
	typ := hdr[0]
	id := binary.BigEndian.Uint32(hdr[1:5])
	l := binary.BigEndian.Uint32(hdr[5:9])
	var payload []byte
	if l > 0 {
		payload = make([]byte, l)
		if _, err := io.ReadFull(rc.br, payload); err != nil {
			return nil, err
		}
	}
	return &frame{typ: typ, streamID: id, payload: payload}, nil
}

// handleFrame processes a received frame
func (rc *relayClient) handleFrame(fr *frame, localAppPort int) {
	switch fr.typ {
	case tOpen:
		rc.handleOpen(fr.streamID, localAppPort)
	case tData:
		rc.handleData(fr.streamID, fr.payload)
	case tClose:
		rc.handleClose(fr.streamID)
	case tRst:
		rc.handleRst(fr.streamID)
	case tPing:
		_ = rc.sendFrame(tPong, 0, fr.payload)
	}
}

// handleOpen handles an OPEN frame
func (rc *relayClient) handleOpen(streamID uint32, localAppPort int) {
	app, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localAppPort))
	if err != nil {
		_ = rc.sendFrame(tRst, streamID, nil)
		return
	}
	rc.streams.Store(streamID, app)
	// pump app -> relay as DATA frames
	go func(id uint32, a net.Conn) {
		defer func() {
			_ = a.Close()
			rc.streams.Delete(id)
		}()
		buf := make([]byte, 64<<10)
		for {
			n, err := a.Read(buf)
			if n > 0 {
				if err := rc.sendFrame(tData, id, buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					_ = rc.sendFrame(tClose, id, nil)
				} else {
					_ = rc.sendFrame(tRst, id, nil)
				}
				return
			}
		}
	}(streamID, app)
}

// handleData handles a DATA frame
func (rc *relayClient) handleData(streamID uint32, payload []byte) {
	if v, ok := rc.streams.Load(streamID); ok {
		a := v.(net.Conn)
		if _, err := a.Write(payload); err != nil {
			_ = rc.sendFrame(tRst, streamID, nil)
			_ = a.Close()
			rc.streams.Delete(streamID)
		}
	} else {
		_ = rc.sendFrame(tRst, streamID, nil)
	}
}

// handleClose handles a CLOSE frame
func (rc *relayClient) handleClose(streamID uint32) {
	if v, ok := rc.streams.Load(streamID); ok {
		if tcp, ok2 := v.(net.Conn).(*net.TCPConn); ok2 {
			_ = tcp.CloseWrite()
		}
	}
}

// handleRst handles a RST frame
func (rc *relayClient) handleRst(streamID uint32) {
	if v, ok := rc.streams.LoadAndDelete(streamID); ok {
		_ = v.(net.Conn).Close()
	}
}

// sendFrame sends a frame to the relay
func (rc *relayClient) sendFrame(typ byte, id uint32, payload []byte) error {
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	var hdr [9]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], id)
	if payload == nil {
		binary.BigEndian.PutUint32(hdr[5:9], 0)
		if _, err := rc.bw.Write(hdr[:]); err != nil {
			return err
		}
		return rc.bw.Flush()
	}
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(payload)))
	if _, err := rc.bw.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := rc.bw.Write(payload); err != nil {
		return err
	}
	return rc.bw.Flush()
}

// Close closes the relay client connection
func (rc *relayClient) Close() error {
	return rc.conn.Close()
}
