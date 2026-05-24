package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Minimal websocket implementation — zero dependencies.
// Supports: text frames, ping/pong, close frames, read/write.

// WebSocket opcodes — raw opcode values per RFC 6455 §5.2.
// FIN and mask bits are added at frame construction time.
const (
	WS_OP_TEXT  = 1
	WS_OP_BIN   = 2
	WS_OP_CLOSE = 8
	WS_OP_PING  = 9
	WS_OP_PONG  = 10
)

type WSConn struct {
	conn io.ReadWriteCloser
	buf  []byte
}

func newWSConn(conn io.ReadWriteCloser) *WSConn {
	return &WSConn{conn: conn, buf: make([]byte, 4096)}
}

// WriteText sends a JSON message as a websocket text frame.
func (c *WSConn) WriteText(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteRaw(data, WS_OP_TEXT)
}

// WriteRaw sends raw bytes as a websocket frame (server-to-client, unmasked).
// Per RFC 6455 §5.2: byte 0 = FIN(1) | RSV(3) | opcode(4); opcode is the LOW nibble.
func (c *WSConn) WriteRaw(data []byte, op int) error {
	var hdr []byte
	b0 := byte(0x80) | byte(op&0x0f) // FIN=1, opcode in low nibble
	if len(data) < 126 {
		hdr = []byte{b0, byte(len(data))}
	} else if len(data) < 65536 {
		hdr = make([]byte, 4)
		hdr[0] = b0
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(len(data)))
	} else {
		hdr = make([]byte, 10)
		hdr[0] = b0
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(len(data)))
	}
	frame := append(hdr, data...)
	_, err := c.conn.Write(frame)
	return err
}

// ReadMessage reads the next websocket frame, returns (opcode, payload).
func (c *WSConn) ReadMessage() (int, []byte, error) {
	hdr, err := c.readFull(2)
	if err != nil {
		return 0, nil, err
	}
	op := int(hdr[0] & 0x0f)
	masked := (hdr[1] & 0x80) != 0
	Len := int(hdr[1] & 0x7f)
	if Len == 126 {
		ext, err := c.readFull(2)
		if err != nil {
			return 0, nil, err
		}
		Len = int(binary.BigEndian.Uint16(ext))
	} else if Len == 127 {
		ext, err := c.readFull(8)
		if err != nil {
			return 0, nil, err
		}
		Len = int(binary.BigEndian.Uint64(ext))
	}

	maskKey := make([]byte, 4)
	if masked {
		buf, err := c.readFull(4)
		if err != nil {
			return 0, nil, err
		}
		copy(maskKey, buf)
	}

	payload := make([]byte, Len)
	buf, err := c.readFull(Len)
	if err != nil {
		return 0, nil, err
	}

	if masked {
		for i := 0; i < Len; i++ {
			payload[i] = buf[i] ^ maskKey[i%4]
		}
	}

	return op, payload, nil
}

func (c *WSConn) readFull(n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(c.conn, buf)
	return buf, err
}

func (c *WSConn) Close() error {
	if rc, ok := c.conn.(interface{ Close() error }); ok {
		return rc.Close()
	}
	return nil
}

// UpgradeHTTP upgrades an HTTP connection to a websocket connection.
func UpgradeHTTP(conn http.ResponseWriter, r *http.Request) (*WSConn, error) {
	// Check upgrade header
	upgrade := r.Header.Get("Upgrade")
	if upgrade != "websocket" {
		return nil, fmt.Errorf("not a websocket request")
	}

	// Get the raw TCP conn from the http.ResponseWriter
	hijacker, ok := conn.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("server does not support hijacking")
	}

	rawConn, _, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack failed: %w", err)
	}

	// Send 101 Switching Protocols
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		fmt.Sprintf("Sec-WebSocket-Accept: %s\r\n\r\n", calcAccept(r.Header.Get("Sec-WebSocket-Key")))

	_, err = rawConn.Write([]byte(resp))
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("response write failed: %w", err)
	}

	return newWSConn(rawConn), nil
}

// calcAccept computes the websocket accept key per RFC 6455.
func calcAccept(key string) string {
	const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(h[:])
}
