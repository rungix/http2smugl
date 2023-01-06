package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/net/context"

	"github.com/lucas-clemente/quic-go"
	"github.com/quic-go/qpack"
)

func sendHTTP3Request(connectAddr, serverName string, noTLS bool, request *HTTPMessage, timeout time.Duration, sslKeylog bool) (response *HTTPMessage, err error) {
	address := connectAddr
	if _, _, err := net.SplitHostPort(connectAddr); err != nil {
		address = net.JoinHostPort(address, "443")
	}

	name, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid address %v: %w", address, err)
	}
	ip, err := DefaultDNSCache.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("lookup for %v failed: %w", name, err)
	}
	portInt, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %w", err)
	}

	udpConn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return nil, err
	}
	defer func() { _ = udpConn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = udpConn.Close()
	}()

	udpAddr := &net.UDPAddr{
		IP:   ip,
		Port: portInt,
	}
	var tlsConfig tls.Config
	if sslKeylog {
		sslKeylogfile, err := os.OpenFile("/tmp/sslkey.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			panic(err)
		}
		defer sslKeylogfile.Close()

		tlsConfig = tls.Config{
			NextProtos:         []string{"h3", "h3-29"},
			ServerName:         serverName,
			InsecureSkipVerify: true,
			KeyLogWriter:       sslKeylogfile,
		}
	} else {
		tlsConfig = tls.Config{
			NextProtos:         []string{"h3", "h3-29"},
			ServerName:         serverName,
			InsecureSkipVerify: true,
		}
	}

	session, err := quic.DialEarlyContext(ctx, udpConn, udpAddr, serverName,
		&tlsConfig,
		&quic.Config{
			Versions:           []quic.VersionNumber{quic.Version1, quic.VersionDraft29},
			MaxIncomingStreams: -1,
			KeepAlivePeriod:    2 * time.Second,
		})

	if err != nil {
		return nil, err
	}
	defer func() { _ = session.CloseWithError(0, "") }()
	if err := setupSession(session); err != nil {
		return nil, err
	}
	requestStream, err := session.OpenStream()
	if err != nil {
		return nil, err
	}
	frames := prepareHTTP3Request(request)
	for _, f := range frames {
		_, _ = requestStream.Write(f)
	}

	if err := requestStream.Close(); err != nil {
		return nil, err
	}

	var (
		headers Headers
		body    []byte
	)
	decoder := qpack.NewDecoder(func(f qpack.HeaderField) {
		headers = append(headers, Header{
			Name:  f.Name,
			Value: f.Value,
		})
	})
	b := bufio.NewReader(requestStream)
	for {
		frame, err := readFrame(b)
		if err != nil {
			if ctx.Err() != nil {
				return nil, TimeoutError{}
			}

			if err == io.EOF {
				break
			}

			if qErr, ok := err.(interface{ IsApplicationError() bool }); ok {
				if qErr.IsApplicationError() {
					return nil, ConnDropError{err}
				}
			}
			return nil, err
		}
		switch frame.Type {
		case 0x0:
			body = append(body, frame.Data...)
		case 0x1:
			if _, err := decoder.Write(frame.Data); err != nil {
				return nil, err
			}
		default:
			// ignore unknown frame types for now
		}
	}

	return &HTTPMessage{
		Headers: headers,
		Body:    body,
	}, nil
}

type http3Frame struct {
	Type int
	Len  uint64
	Data []byte
}

func readFrame(b *bufio.Reader) (*http3Frame, error) {
	t, err := readVarInt(b)
	if err != nil {
		return nil, err
	}
	l, err := readVarInt(b)
	if err != nil {
		return nil, err
	}
	data := make([]byte, l)
	if _, err := io.ReadFull(b, data); err != nil {
		return nil, err
	}
	return &http3Frame{
		Type: int(t),
		Len:  l,
		Data: data,
	}, nil
}

func prepareHTTP3Request(request *HTTPMessage) [][]byte {
	frames := [][]byte{encodeHeaders(request.Headers)}
	frames = append(frames, encodeBody(request.Body)...)
	return frames
}

func encodeHeaders(headers Headers) []byte {
	qpackBuf := bytes.NewBuffer(nil)
	e := qpack.NewEncoder(qpackBuf)
	for _, h := range headers {
		_ = e.WriteField(qpack.HeaderField{Name: h.Name, Value: h.Value})
	}
	headersFrame := bytes.NewBuffer(nil)
	writeVarInt(headersFrame, 0x1)
	writeVarInt(headersFrame, uint64(qpackBuf.Len()))
	headersFrame.Write(qpackBuf.Bytes())
	return headersFrame.Bytes()
}

func encodeBody(body []byte) (frames [][]byte) {
	if len(body) == 0 {
		return nil
	}
	buf := bytes.NewBuffer(nil)
	writeVarInt(buf, 0x00)
	writeVarInt(buf, uint64(len(body)))
	buf.Write(body)
	return [][]byte{buf.Bytes()}
}

func setupSession(session quic.EarlyConnection) error {
	str, err := session.OpenUniStream()
	if err != nil {
		return err
	}
	buf := &bytes.Buffer{}
	buf.Write([]byte{0x0, 0x4, 0x0}) // TODO: this is shit
	if _, err := str.Write(buf.Bytes()); err != nil {
		return err
	}
	return nil
}

const (
	maxVarInt1 = 63
	maxVarInt2 = 16383
	maxVarInt4 = 1073741823
	maxVarInt8 = 4611686018427387903
)

func readVarInt(b io.ByteReader) (uint64, error) {
	firstByte, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	// the first two bits of the first byte encode the length
	intLen := 1 << ((firstByte & 0xc0) >> 6)
	b1 := firstByte & (0xff - 0xc0)
	if intLen == 1 {
		return uint64(b1), nil
	}
	b2, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	if intLen == 2 {
		return uint64(b2) + uint64(b1)<<8, nil
	}
	b3, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	b4, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	if intLen == 4 {
		return uint64(b4) + uint64(b3)<<8 + uint64(b2)<<16 + uint64(b1)<<24, nil
	}
	b5, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	b6, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	b7, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	b8, err := b.ReadByte()
	if err != nil {
		return 0, err
	}
	return uint64(b8) + uint64(b7)<<8 + uint64(b6)<<16 + uint64(b5)<<24 + uint64(b4)<<32 + uint64(b3)<<40 + uint64(b2)<<48 + uint64(b1)<<56, nil
}

func writeVarInt(b *bytes.Buffer, i uint64) {
	if i <= maxVarInt1 {
		b.WriteByte(uint8(i))
	} else if i <= maxVarInt2 {
		b.Write([]byte{uint8(i>>8) | 0x40, uint8(i)})
	} else if i <= maxVarInt4 {
		b.Write([]byte{uint8(i>>24) | 0x80, uint8(i >> 16), uint8(i >> 8), uint8(i)})
	} else if i <= maxVarInt8 {
		b.Write([]byte{
			uint8(i>>56) | 0xc0, uint8(i >> 48), uint8(i >> 40), uint8(i >> 32),
			uint8(i >> 24), uint8(i >> 16), uint8(i >> 8), uint8(i),
		})
	} else {
		panic(fmt.Sprintf("%#x doesn't fit into 62 bits", i))
	}
}
