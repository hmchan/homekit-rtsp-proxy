package hap

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	packetSizeMax = 0x400 // 1024 bytes max plaintext per frame
	authTagSize   = 16    // Poly1305 auth tag
)

// EncryptedConn wraps a net.Conn with HAP session encryption (ChaCha20-Poly1305).
// After pair-verify, all HTTP traffic flows through this encrypted layer.
type EncryptedConn struct {
	conn net.Conn
	rw   *bufio.ReadWriter

	encryptKey [32]byte
	decryptKey [32]byte
	encryptCnt uint64
	decryptCnt uint64

	wmu sync.Mutex

	// readBuf holds decrypted data from the current frame that hasn't been
	// consumed yet by the caller.
	readBuf []byte
}

// NewEncryptedConn wraps an existing connection with HAP session encryption.
// readKey and writeKey are derived from pair-verify's shared secret.
// IMPORTANT: "Read" and "Write" keys are named from the accessory's perspective.
// As a client (controller), we encrypt with writeKey and decrypt with readKey.
func NewEncryptedConn(conn net.Conn, readKey, writeKey [32]byte) *EncryptedConn {
	return &EncryptedConn{
		conn: conn,
		rw: bufio.NewReadWriter(
			bufio.NewReader(conn),
			bufio.NewWriter(conn),
		),
		encryptKey: writeKey, // We encrypt with "Write" key (accessory reads with it)
		decryptKey: readKey,  // We decrypt with "Read" key (accessory writes with it)
	}
}

// Write encrypts and sends data, chunking into 1024-byte frames.
func (c *EncryptedConn) Write(b []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	aead, err := chacha20poly1305.New(c.encryptKey[:])
	if err != nil {
		return 0, fmt.Errorf("create AEAD: %w", err)
	}

	total := 0
	for len(b) > 0 {
		size := len(b)
		if size > packetSizeMax {
			size = packetSizeMax
		}

		// 2-byte LE length (plaintext, used as AAD)
		var lengthBytes [2]byte
		binary.LittleEndian.PutUint16(lengthBytes[:], uint16(size))
		if _, err := c.rw.Write(lengthBytes[:]); err != nil {
			return total, fmt.Errorf("write length: %w", err)
		}

		// Build nonce: 4 zero bytes + 8-byte LE counter
		nonce := make([]byte, chacha20poly1305.NonceSize) // 12 bytes
		binary.LittleEndian.PutUint64(nonce[4:], c.encryptCnt)
		c.encryptCnt++

		// Encrypt with AAD = length bytes
		ciphertext := aead.Seal(nil, nonce, b[:size], lengthBytes[:])

		if _, err := c.rw.Write(ciphertext); err != nil {
			return total, fmt.Errorf("write ciphertext: %w", err)
		}

		b = b[size:]
		total += size
	}

	if err := c.rw.Flush(); err != nil {
		return total, fmt.Errorf("flush: %w", err)
	}
	return total, nil
}

// Read decrypts data from the encrypted connection.
func (c *EncryptedConn) Read(b []byte) (int, error) {
	// Return buffered data from a previous frame first.
	if len(c.readBuf) > 0 {
		n := copy(b, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}

	aead, err := chacha20poly1305.New(c.decryptKey[:])
	if err != nil {
		return 0, fmt.Errorf("create AEAD: %w", err)
	}

	// Read 2-byte plaintext length.
	var lengthBytes [2]byte
	if _, err := io.ReadFull(c.rw, lengthBytes[:]); err != nil {
		return 0, err
	}
	plaintextLen := int(binary.LittleEndian.Uint16(lengthBytes[:]))

	// Read ciphertext + auth tag.
	ciphertext := make([]byte, plaintextLen+authTagSize)
	if _, err := io.ReadFull(c.rw, ciphertext); err != nil {
		return 0, fmt.Errorf("read ciphertext: %w", err)
	}

	// Build nonce.
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.LittleEndian.PutUint64(nonce[4:], c.decryptCnt)
	c.decryptCnt++

	// Decrypt with AAD = length bytes.
	plaintext, err := aead.Open(nil, nonce, ciphertext, lengthBytes[:])
	if err != nil {
		return 0, fmt.Errorf("decrypt frame: %w", err)
	}

	n := copy(b, plaintext)
	if n < len(plaintext) {
		c.readBuf = plaintext[n:]
	}
	return n, nil
}

// Close closes the underlying connection.
func (c *EncryptedConn) Close() error {
	return c.conn.Close()
}

// LocalAddr returns the local network address.
func (c *EncryptedConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *EncryptedConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}
