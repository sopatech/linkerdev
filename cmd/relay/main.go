package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

/*
Framed protocol (big-endian):
  byte  type
  u32   streamID
  u32   length
  bytes payload (length)

Types:
  0x01 OPEN    (server->client) : begin new inbound stream
  0x02 DATA    (both directions) : payload bytes
  0x03 CLOSE   (both directions) : half-close (FIN); no payload
  0x04 RST     (both directions) : abort; no payload
  0x05 OUTBOUND_CONNECT (client->server) : begin new outbound stream with destination
  0x10 PING    (either)          : keepalive; optional payload
  0x11 PONG    (either)          : reply to PING

Server behavior:
- Accepts one CONTROL connection on --control (TCP).
- Accepts many inbound connections on --listen (TCP). For each:
    * allocate streamID, send OPEN, then forward bytes as DATA frames.
- From CONTROL, DATA/CLOSE/RST frames are demuxed to the right stream.
- If CONTROL drops: all streams are closed & reset.
*/

var version = "v0.1.0-alpha16"

const (
	tOpen            = 0x01
	tData            = 0x02
	tClose           = 0x03
	tRst             = 0x04
	tOutboundConnect = 0x05
	tPing            = 0x10
	tPong            = 0x11

	writeBuf = 64 << 10 // 64KiB framing buffer
)

type frame struct {
	typ      byte
	streamID uint32
	payload  []byte
}

type stream struct {
	id     uint32
	conn   net.Conn
	closed int32 // atomic bool
}

type relayServer struct {
	ctrlAddr   string
	listenAddr string

	mu       sync.Mutex
	ctrlConn net.Conn
	writer   *bufio.Writer
	writeMu  sync.Mutex // serialize writes on ctrlConn

	nextID  uint32
	streams sync.Map // id -> *stream

	closing int32 // server shutting down
}

func main() {
	var (
		control     = flag.Int("control", 18080, "control TCP port")
		listen      = flag.Int("listen", 20000, "reverse/listen TCP port (cluster inbound)")
		showVersion = flag.Bool("version", false, "show version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("linkerdev-relay version %s\n", version)
		os.Exit(0)
	}

	rs := &relayServer{
		ctrlAddr:   fmt.Sprintf("0.0.0.0:%d", *control),
		listenAddr: fmt.Sprintf("0.0.0.0:%d", *listen),
	}

	log.Printf("linkerdev-relay v%s starting: control=%s listen=%s", version, rs.ctrlAddr, rs.listenAddr)

	// Start control listener (handles both TCP and HTTP on same port)
	go rs.runControlListener()

	// Start reverse listener (cluster -> local)
	if err := rs.runListen(); err != nil {
		log.Fatalf("listen failed: %v", err)
	}
}

func (rs *relayServer) runControlListener() {
	ln, err := net.Listen("tcp", rs.ctrlAddr)
	if err != nil {
		log.Fatalf("control listen: %v", err)
	}
	defer ln.Close()

	log.Printf("control listener on %s", rs.ctrlAddr)
	for {
		c, err := ln.Accept()
		if err != nil {
			if atomic.LoadInt32(&rs.closing) == 1 {
				return
			}
			log.Printf("control accept: %v", err)
			continue
		}

		// Check if this is an HTTP request by peeking at the first few bytes
		c.SetReadDeadline(time.Now().Add(1 * time.Second))
		buf := make([]byte, 4)
		n, err := c.Read(buf)
		c.SetReadDeadline(time.Time{}) // Clear deadline

		if err == nil && n >= 4 && string(buf[:4]) == "GET " {
			// This is an HTTP request, handle it
			go rs.handleHTTP(c, buf)
		} else {
			// This is a TCP control connection, handle normally
			go rs.handleControl(c)
		}
	}
}

func (rs *relayServer) handleHTTP(conn net.Conn, firstBytes []byte) {
	defer conn.Close()

	// Read the rest of the HTTP request
	reader := bufio.NewReader(io.MultiReader(
		strings.NewReader(string(firstBytes)),
		conn,
	))

	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("HTTP request error: %v", err)
		return
	}

	// Handle health check
	if req.URL.Path == "/healthz" {
		response := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\n\r\nOK"
		conn.Write([]byte(response))
		return
	}

	// Handle other requests with 404
	response := "HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\n\r\nNot Found"
	conn.Write([]byte(response))
}

func (rs *relayServer) handleControl(c net.Conn) {
	log.Printf("control connected from %s", c.RemoteAddr())
	bw := bufio.NewWriterSize(c, writeBuf)

	rs.mu.Lock()
	// Close previous control connection (drops all streams)
	if rs.ctrlConn != nil {
		_ = rs.ctrlConn.Close()
	}
	rs.ctrlConn = c
	rs.writer = bw
	rs.mu.Unlock()

	// Drop all streams outside the mutex to avoid deadlock
	rs.dropAllStreams()

	// Reader loop
	br := bufio.NewReader(c)
	for {
		fr, err := readFrame(br)
		if err != nil {
			if err != io.EOF {
				log.Printf("control read: %v", err)
			}
			break
		}
		switch fr.typ {
		case tPong, tPing:
			// ignore; keepalive handled elsewhere if needed
		case tData:
			if st, ok := rs.streamByID(fr.streamID); ok {
				if !st.writeToLocal(fr.payload) {
					// could not write -> drop stream
					rs.sendFrame(tRst, fr.streamID, nil)
					rs.closeStream(st.id)
				}
			} else {
				// stream not found; inform client
				rs.sendFrame(tRst, fr.streamID, nil)
			}
		case tClose:
			if st, ok := rs.streamByID(fr.streamID); ok {
				_ = st.conn.(*net.TCPConn).CloseWrite()
			}
		case tRst:
			rs.closeStream(fr.streamID)
		case tOutboundConnect:
			rs.handleOutboundConnect(fr.streamID, string(fr.payload))
		default:
			// unknown; ignore
		}
	}
	// control dropped
	rs.mu.Lock()
	if rs.ctrlConn == c {
		rs.ctrlConn = nil
		rs.writer = nil
	}
	rs.mu.Unlock()

	rs.dropAllStreams()
	_ = c.Close()
	log.Printf("control disconnected")
}

func (rs *relayServer) runListen() error {
	ln, err := net.Listen("tcp", rs.listenAddr)
	if err != nil {
		return err
	}
	defer ln.Close()
	go rs.pinger()

	for {
		cc, err := ln.Accept()
		if err != nil {
			if atomic.LoadInt32(&rs.closing) == 1 {
				return nil
			}
			log.Printf("listen accept: %v", err)
			continue
		}
		// Require active control conn
		if !rs.hasControl() {
			log.Printf("no control connection; rejecting inbound from %s", cc.RemoteAddr())
			_ = cc.Close()
			continue
		}
		go rs.handleInbound(cc)
	}
}

func (rs *relayServer) handleOutboundConnect(streamID uint32, destination string) {
	log.Printf("outbound connect request: streamID=%d, destination=%s", streamID, destination)

	// Connect to the destination service in the cluster
	conn, err := net.Dial("tcp", destination)
	if err != nil {
		log.Printf("failed to connect to %s: %v", destination, err)
		rs.sendFrame(tRst, streamID, nil)
		return
	}

	// Create stream and store it
	st := &stream{id: streamID, conn: conn}
	rs.streams.Store(streamID, st)

	// Start forwarding data from cluster to local
	go func() {
		defer rs.closeStream(streamID)
		buf := make([]byte, 64<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				rs.sendFrame(tData, streamID, buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					rs.sendFrame(tRst, streamID, nil)
				} else {
					rs.sendFrame(tClose, streamID, nil)
				}
				return
			}
		}
	}()

	log.Printf("outbound connection established: streamID=%d, destination=%s", streamID, destination)
}

func (rs *relayServer) handleInbound(c net.Conn) {
	id := rs.allocID()
	st := &stream{id: id, conn: c}
	rs.streams.Store(id, st)

	// notify client: OPEN
	rs.sendFrame(tOpen, id, nil)

	// pump data to control
	go func() {
		defer rs.closeStream(id) // Ensure stream is always cleaned up
		buf := make([]byte, 64<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				rs.sendFrame(tData, id, buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					rs.sendFrame(tRst, id, nil)
				} else {
					rs.sendFrame(tClose, id, nil)
				}
				return
			}
		}
	}()
	// The reverse direction (control -> local) is handled in control reader (tData/tClose)
}

func (rs *relayServer) pinger() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
		if rs.hasControl() {
			_ = rs.sendFrame(tPing, 0, []byte("hi"))
		}
	}
}

/* ---------- helpers ---------- */

func (rs *relayServer) hasControl() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.ctrlConn != nil && rs.writer != nil
}

func (rs *relayServer) allocID() uint32 {
	for {
		id := atomic.AddUint32(&rs.nextID, 1)
		if id != 0 {
			return id
		}
		// If we wrapped around to 0, try again
		// This is extremely unlikely in practice but ensures correctness
	}
}

func (rs *relayServer) streamByID(id uint32) (*stream, bool) {
	v, ok := rs.streams.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*stream), true
}

func (rs *relayServer) closeStream(id uint32) {
	if v, ok := rs.streams.LoadAndDelete(id); ok {
		st := v.(*stream)
		_ = st.conn.Close()
	}
}

func (rs *relayServer) dropAllStreams() {
	rs.streams.Range(func(key, value any) bool {
		st := value.(*stream)
		_ = st.conn.Close()
		rs.streams.Delete(key)
		return true
	})
}

func (rs *relayServer) sendFrame(typ byte, id uint32, payload []byte) error {
	rs.mu.Lock()
	w := rs.writer
	rs.mu.Unlock()
	if w == nil {
		return io.ErrClosedPipe
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	var hdr [9]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], id)
	if payload == nil {
		binary.BigEndian.PutUint32(hdr[5:9], 0)
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		return w.Flush()
	}
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}

func readFrame(r *bufio.Reader) (*frame, error) {
	var hdr [9]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	typ := hdr[0]
	id := binary.BigEndian.Uint32(hdr[1:5])
	l := binary.BigEndian.Uint32(hdr[5:9])
	var payload []byte
	if l > 0 {
		payload = make([]byte, l)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return &frame{typ: typ, streamID: id, payload: payload}, nil
}

/* stream helpers */

func (s *stream) writeToLocal(p []byte) bool {
	if atomic.LoadInt32(&s.closed) == 1 {
		return false
	}
	_, err := s.conn.Write(p)
	return err == nil
}

/* graceful shutdown (optional) */
func init() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}
