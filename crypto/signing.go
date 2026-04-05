package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const (
	PayloadSize   = 40  // 8+8+16+8 bytes
	SignatureSize = 64  // Ed25519 signature
	TotalSize     = PayloadSize + SignatureSize
	TokenExpiryMs = 60_000
)

type Signer struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// NewSigner creates a signer from a base64-encoded Ed25519 private key (64 bytes raw).
func NewSigner(base64PrivKey string) (*Signer, error) {
	privBytes, err := base64.StdEncoding.DecodeString(base64PrivKey)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}

	if len(privBytes) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid private key size: got %d, want %d", len(privBytes), ed25519.PrivateKeySize)
	}

	priv := ed25519.PrivateKey(privBytes)
	pub := priv.Public().(ed25519.PublicKey)

	return &Signer{privateKey: priv, publicKey: pub}, nil
}

// GenerateKeypair creates a new Ed25519 keypair for development/setup.
func GenerateKeypair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	return pub, priv, err
}

// SignToken creates a signed 104-byte token.
// Payload (40 bytes): serverTimestamp(8) + expiry(8) + deviceHash(16) + nonce(8)
// Signature (64 bytes): Ed25519(privateKey, payload)
func (s *Signer) SignToken(deviceHash []byte) ([]byte, int64, error) {
	now := time.Now().UnixMilli()
	expiry := now + TokenExpiryMs

	// Generate 8-byte nonce
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, fmt.Errorf("generate nonce: %w", err)
	}

	// Truncate or pad device hash to 16 bytes
	dh := make([]byte, 16)
	copy(dh, deviceHash)

	// Build payload (40 bytes)
	payload := make([]byte, PayloadSize)
	binary.BigEndian.PutUint64(payload[0:8], uint64(now))
	binary.BigEndian.PutUint64(payload[8:16], uint64(expiry))
	copy(payload[16:32], dh)
	copy(payload[32:40], nonce)

	// Sign
	signature := ed25519.Sign(s.privateKey, payload)

	// Combine: payload + signature = 104 bytes
	token := make([]byte, TotalSize)
	copy(token[0:PayloadSize], payload)
	copy(token[PayloadSize:TotalSize], signature)

	return token, now, nil
}

// PublicKeyBytes returns the raw 32-byte Ed25519 public key.
func (s *Signer) PublicKeyBytes() []byte {
	return []byte(s.publicKey)
}

// FormatKotlinByteArray renders a byte slice as a Kotlin `byteArrayOf(...)`
// literal with signed-byte values (Kotlin bytes are int8, so values > 127
// are printed as their negative two's-complement equivalent). 8 bytes per
// line, matching the existing Android TokenVerifier.SERVER_PUBLIC_KEY
// layout so a deploy log line can be byte-compared against the committed
// constant without reformatting.
func FormatKotlinByteArray(b []byte) string {
	var sb strings.Builder
	sb.WriteString("byteArrayOf(\n    ")
	parts := make([]string, len(b))
	for i, x := range b {
		if x > 127 {
			parts[i] = fmt.Sprintf("%d", int(x)-256)
		} else {
			parts[i] = fmt.Sprintf("%d", x)
		}
	}
	for i := 0; i < len(parts); i += 8 {
		end := i + 8
		if end > len(parts) {
			end = len(parts)
		}
		sb.WriteString(strings.Join(parts[i:end], ", "))
		if end < len(parts) {
			sb.WriteString(",\n    ")
		} else {
			sb.WriteString("\n")
		}
	}
	sb.WriteString(")")
	return sb.String()
}
