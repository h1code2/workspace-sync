package syncer

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

func dialWithAuth(ctx context.Context, addr, token string) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if err := clientAuth(conn, token); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func clientAuth(conn net.Conn, token string) error {
	challenge := make([]byte, 32)
	if _, err := io.ReadFull(conn, challenge); err != nil {
		return err
	}
	sig := sign(token, challenge)
	if _, err := conn.Write(sig); err != nil {
		return err
	}
	ack := make([]byte, 1)
	if _, err := io.ReadFull(conn, ack); err != nil {
		return err
	}
	if ack[0] != 1 {
		return fmt.Errorf("auth failed")
	}
	return nil
}

func serverAuth(conn net.Conn, token string) error {
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return err
	}
	if _, err := conn.Write(challenge); err != nil {
		return err
	}
	recv := make([]byte, 32)
	if _, err := io.ReadFull(conn, recv); err != nil {
		return err
	}
	expected := sign(token, challenge)
	ok := subtle.ConstantTimeCompare(expected, recv) == 1
	var b byte
	if ok {
		b = 1
	}
	if _, err := conn.Write([]byte{b}); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("auth reject")
	}
	return nil
}

func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > 64*1024*1024 {
		return fmt.Errorf("payload too large")
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header)
	if n == 0 || n > 64*1024*1024 {
		return nil, fmt.Errorf("invalid frame size: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func encodeID(rel string, modTime int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", rel, modTime)))
}
