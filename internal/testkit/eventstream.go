package testkit

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// The AWS event-stream encoding (application/vnd.amazon.eventstream), with string headers only:
// total length, headers length, prelude CRC, headers, payload, message CRC. Written here so the
// testkit needs no AWS module.

// esMessage is one event-stream message.
type esMessage struct {
	headers [][2]string
	payload []byte
}

const headerString = 7

func (m esMessage) encode(w io.Writer) error {
	var h bytes.Buffer
	for _, kv := range m.headers {
		h.WriteByte(byte(len(kv[0])))
		h.WriteString(kv[0])
		h.WriteByte(headerString)
		_ = binary.Write(&h, binary.BigEndian, uint16(len(kv[1])))
		h.WriteString(kv[1])
	}
	total := 12 + h.Len() + len(m.payload) + 4
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(total))
	_ = binary.Write(&b, binary.BigEndian, uint32(h.Len()))
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	b.Write(h.Bytes())
	b.Write(m.payload)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	_, err := w.Write(b.Bytes())
	return err
}

// decodeMessage reads one message; io.EOF at a clean end.
func decodeMessage(r io.Reader) (esMessage, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(r, prelude[:]); err != nil {
		return esMessage{}, err
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) || total < 16+headersLen {
		return esMessage{}, errors.New("eventstream: bad prelude")
	}
	rest := make([]byte, total-12)
	if _, err := io.ReadFull(r, rest); err != nil {
		return esMessage{}, err
	}
	body := rest[:len(rest)-4]
	if crc32.ChecksumIEEE(append(prelude[:], body...)) != binary.BigEndian.Uint32(rest[len(rest)-4:]) {
		return esMessage{}, errors.New("eventstream: bad message checksum")
	}
	var m esMessage
	h := body[:headersLen]
	for len(h) > 0 {
		n := int(h[0])
		if len(h) < 1+n+3 || h[1+n] != headerString {
			return esMessage{}, fmt.Errorf("eventstream: unsupported header")
		}
		name := string(h[1 : 1+n])
		vl := int(binary.BigEndian.Uint16(h[2+n : 4+n]))
		if len(h) < 4+n+vl {
			return esMessage{}, errors.New("eventstream: short header")
		}
		m.headers = append(m.headers, [2]string{name, string(h[4+n : 4+n+vl])})
		h = h[4+n+vl:]
	}
	m.payload = body[headersLen:]
	return m, nil
}
