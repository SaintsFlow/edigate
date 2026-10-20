// Description pins of upstream tools: the canonical form of a tool description, its
// SHA-256, and the ed25519 signature of the administrator over it (decision D6).
//
// A pin says: "this exact description of this tool of this server was read and approved".
// The description is the object {name, description, inputSchema, outputSchema,
// annotations} that the upstream sent, written in the canonical form of RFC 8785.
// Any change to any of those five members changes the hash.
package upstream

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gowebpki/jcs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// description is what is hashed. Absent members are written as null, so the shape
// is fixed.
type description struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	InputSchema  any    `json:"inputSchema"`
	OutputSchema any    `json:"outputSchema"`
	Annotations  any    `json:"annotations"`
}

// Hash returns the SHA-256 of the canonical description of a tool.
func Hash(t *mcp.Tool) ([32]byte, error) {
	d := description{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, OutputSchema: t.OutputSchema}
	if t.Annotations != nil {
		d.Annotations = t.Annotations
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return [32]byte{}, err
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// signedMessage is what the administrator signs: the server name and the hash, so a pin
// of one server cannot be moved to another.
func signedMessage(server string, hash [32]byte) []byte {
	return append([]byte("edigate-manifest-v1\x00"+server+"\x00"), hash[:]...)
}

// Sign signs the pin of a tool of a server.
func Sign(priv ed25519.PrivateKey, server string, hash [32]byte) []byte {
	return ed25519.Sign(priv, signedMessage(server, hash))
}

// Verify reports whether sig is the signature of the key owner over the pin.
func Verify(pub ed25519.PublicKey, server string, hash [32]byte, sig []byte) bool {
	return len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, signedMessage(server, hash), sig)
}

// GenerateKey makes a new key pair and returns both halves in base64.
func GenerateKey() (pub, priv string, err error) {
	p, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(p), base64.StdEncoding.EncodeToString(k), nil
}

// ParsePublicKey reads the base64 form of a public key (EDIGATE_MANIFEST_PUBKEY).
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("the public key must be 32 bytes in base64")
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey reads the base64 form of a private key: 64 bytes, or a 32 byte seed.
func ParsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("the private key is not base64: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	}
	return nil, errors.New("the private key must be 64 bytes (or a 32 byte seed) in base64")
}
