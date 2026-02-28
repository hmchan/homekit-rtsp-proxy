package hap

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// VerifiedConn holds the result of a successful pair-verify handshake.
type VerifiedConn struct {
	Conn      net.Conn
	ReadKey   [32]byte
	WriteKey  [32]byte
	Encrypted *EncryptedConn // Encrypted wrapper for HAP communication
	Client    *HAPClient     // HAP client for characteristic access
}

// DoPairVerify performs the HAP pair-verify handshake over a fresh TCP connection.
// This implementation correctly omits the Method TLV in M1, following the HAP spec
// and go2rtc's battle-tested approach.
func DoPairVerify(addr string, controllerID string, controllerLTSK ed25519.PrivateKey, controllerLTPK ed25519.PublicKey, accessoryLTPK ed25519.PublicKey) (*VerifiedConn, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	vc, err := doPairVerifyOnConn(conn, controllerID, controllerLTSK, controllerLTPK, accessoryLTPK)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return vc, nil
}

func doPairVerifyOnConn(conn net.Conn, controllerID string, controllerLTSK ed25519.PrivateKey, controllerLTPK, accessoryLTPK ed25519.PublicKey) (*VerifiedConn, error) {
	// Generate ephemeral Curve25519 keypair.
	var localPrivate [32]byte
	if _, err := io.ReadFull(rand.Reader, localPrivate[:]); err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	localPublic, err := curve25519.X25519(localPrivate[:], curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("compute public key: %w", err)
	}

	// === M1: Send State + PublicKey (NO Method field) ===
	m1 := TLV8Encode([]TLV8Item{
		{Type: 0x06, Value: []byte{0x01}},  // State = M1
		{Type: 0x03, Value: localPublic},    // PublicKey
	})

	m2Body, err := hapPost(conn, "/pair-verify", m1)
	if err != nil {
		return nil, fmt.Errorf("M1 request: %w", err)
	}

	// === M2: Parse response ===
	items, err := TLV8Decode(m2Body)
	if err != nil {
		return nil, fmt.Errorf("decode M2: %w", err)
	}

	if errCode, ok := TLV8GetByte(items, 0x07); ok && errCode != 0 {
		return nil, fmt.Errorf("M2 error code %d", errCode)
	}

	state, _ := TLV8GetByte(items, 0x06)
	if state != 0x02 {
		return nil, fmt.Errorf("M2 unexpected state %d (expected 2, body %d bytes)", state, len(m2Body))
	}

	remotePubKey := TLV8GetBytes(items, 0x03) // Accessory's ephemeral public key
	encData := TLV8GetBytes(items, 0x05)       // Encrypted data

	if len(remotePubKey) != 32 {
		return nil, fmt.Errorf("M2 bad public key length %d", len(remotePubKey))
	}
	if len(encData) < 16 {
		return nil, fmt.Errorf("M2 encrypted data too short (%d bytes)", len(encData))
	}

	// Compute shared secret.
	sharedSecret, err := curve25519.X25519(localPrivate[:], remotePubKey)
	if err != nil {
		return nil, fmt.Errorf("compute shared secret: %w", err)
	}

	// Derive encryption key for verify session.
	verifyKey := hkdfSha512(sharedSecret, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")

	// Decrypt M2 sub-TLV.
	decrypted, err := aeadDecrypt(verifyKey[:], "PV-Msg02", encData)
	if err != nil {
		return nil, fmt.Errorf("decrypt M2: %w", err)
	}

	subItems, err := TLV8Decode(decrypted)
	if err != nil {
		return nil, fmt.Errorf("decode M2 sub-TLV: %w", err)
	}

	accessoryIdent := TLV8GetBytes(subItems, 0x01)
	signature := TLV8GetBytes(subItems, 0x0A)
	if signature == nil {
		return nil, fmt.Errorf("M2 missing signature")
	}

	// Verify accessory's Ed25519 signature.
	var verifyMaterial []byte
	verifyMaterial = append(verifyMaterial, remotePubKey...)
	verifyMaterial = append(verifyMaterial, accessoryIdent...)
	verifyMaterial = append(verifyMaterial, localPublic...)

	if !ed25519.Verify(accessoryLTPK, verifyMaterial, signature) {
		return nil, fmt.Errorf("M2 signature invalid (accessoryIdent=%q, ltpk=%x, sig_len=%d, material_len=%d)",
			string(accessoryIdent), accessoryLTPK[:8], len(signature), len(verifyMaterial))
	}

	// === M3: Sign and encrypt our identity ===
	var signMaterial []byte
	signMaterial = append(signMaterial, localPublic...)
	signMaterial = append(signMaterial, []byte(controllerID)...)
	signMaterial = append(signMaterial, remotePubKey...)

	m3sig := ed25519.Sign(controllerLTSK, signMaterial)

	m3sub := TLV8Encode([]TLV8Item{
		{Type: 0x01, Value: []byte(controllerID)}, // Identifier
		{Type: 0x0A, Value: m3sig},                 // Signature
	})

	encrypted, err := aeadEncrypt(verifyKey[:], "PV-Msg03", m3sub)
	if err != nil {
		return nil, fmt.Errorf("encrypt M3: %w", err)
	}

	m3 := TLV8Encode([]TLV8Item{
		{Type: 0x06, Value: []byte{0x03}},  // State = M3
		{Type: 0x05, Value: encrypted},      // EncryptedData
	})

	m4Body, err := hapPost(conn, "/pair-verify", m3)
	if err != nil {
		return nil, fmt.Errorf("M3 request: %w", err)
	}

	// === M4: Check response ===
	m4Items, err := TLV8Decode(m4Body)
	if err != nil {
		return nil, fmt.Errorf("decode M4: %w", err)
	}

	if errCode, ok := TLV8GetByte(m4Items, 0x07); ok && errCode != 0 {
		return nil, fmt.Errorf("M4 error code %d", errCode)
	}

	// Derive session encryption keys.
	readKey := hkdfSha512(sharedSecret, "Control-Salt", "Control-Read-Encryption-Key")
	writeKey := hkdfSha512(sharedSecret, "Control-Salt", "Control-Write-Encryption-Key")

	// Clear deadlines set during pair-verify before wrapping with encryption.
	conn.SetDeadline(time.Time{})

	// Create encrypted connection wrapper and HAP client.
	enc := NewEncryptedConn(conn, readKey, writeKey)
	client := NewHAPClient(enc)

	return &VerifiedConn{
		Conn:      conn,
		ReadKey:   readKey,
		WriteKey:  writeKey,
		Encrypted: enc,
		Client:    client,
	}, nil
}

// hapPost sends an HTTP POST with application/pairing+tlv8 content type over a raw connection.
func hapPost(conn net.Conn, path string, body []byte) ([]byte, error) {
	reqBuf := &bytes.Buffer{}
	fmt.Fprintf(reqBuf, "POST %s HTTP/1.1\r\n", path)
	fmt.Fprintf(reqBuf, "Host: %s\r\n", conn.RemoteAddr().String())
	fmt.Fprintf(reqBuf, "Content-Type: application/pairing+tlv8\r\n")
	fmt.Fprintf(reqBuf, "Content-Length: %d\r\n", len(body))
	fmt.Fprintf(reqBuf, "\r\n")
	reqBuf.Write(body)

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(reqBuf.Bytes()); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// hkdfSha512 derives a 32-byte key using HKDF-SHA512.
func hkdfSha512(secret []byte, salt, info string) [32]byte {
	var key [32]byte
	r := hkdf.New(sha512.New, secret, []byte(salt), []byte(info))
	io.ReadFull(r, key[:])
	return key
}

// aeadDecrypt decrypts using ChaCha20-Poly1305 with the HAP nonce scheme.
func aeadDecrypt(key []byte, nonceStr string, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	// HAP nonce: 4 zero bytes + nonce string (up to 8 bytes, padded right).
	copy(nonce[4:], []byte(nonceStr))
	return aead.Open(nil, nonce, ciphertext, nil)
}

// aeadEncrypt encrypts using ChaCha20-Poly1305 with the HAP nonce scheme.
func aeadEncrypt(key []byte, nonceStr string, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	copy(nonce[4:], []byte(nonceStr))
	return aead.Seal(nil, nonce, plaintext, nil), nil
}
