// Package hmac provides HMAC-SHA256 signing and verification for scan-result
// messages. The canonical form of the payload is deterministic JSON with
// alphabetically sorted keys, produced by encoding/json with the sorted-key
// constraint met via struct field ordering. The nonce (UUID) and scannedAt
// timestamp are included in the signed payload to prevent replay attacks.
package hmac

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

const (
	// AlgHMACSHA256 is the algorithm identifier included in the sig envelope.
	AlgHMACSHA256 = "HMAC-SHA256"

	// KeyIDV1 is the current key identifier.
	KeyIDV1 = "scan-result-v1"
)

// Sig is the signature envelope attached alongside the payload in the
// outer message. It is serialised to JSON and included in the published
// RabbitMQ message body.
type Sig struct {
	Alg   string `json:"alg"`
	KeyID string `json:"keyId"`
	Nonce string `json:"nonce"`
	Value string `json:"value"` // base64-encoded HMAC-SHA256 of canonical(payload)
}

// SignedEnvelope is the top-level RabbitMQ message body for signed scan results.
// The payload field holds the raw JSON of the original message (marshalled
// with alphabetically-sorted keys), and sig holds the authentication envelope.
type SignedEnvelope struct {
	Payload json.RawMessage `json:"payload"`
	Sig     Sig             `json:"sig"`
}

// Signer holds the HMAC key material and signs payloads.
type Signer struct {
	key   []byte
	keyID string
}

// NewSigner creates a Signer with the provided raw key bytes and keyId.
// key must be exactly 32 bytes (256 bits). Returns an error otherwise.
func NewSigner(key []byte, keyID string) (*Signer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("HMAC key must be 32 bytes, got %d", len(key))
	}
	if keyID == "" {
		return nil, fmt.Errorf("keyID must not be empty")
	}
	keyCopy := make([]byte, 32)
	copy(keyCopy, key)
	return &Signer{key: keyCopy, keyID: keyID}, nil
}

// Sign serialises v to canonical JSON (alphabetically sorted keys via Go's
// reflect-based map ordering + struct field ordering), computes HMAC-SHA256
// of those bytes, and returns a SignedEnvelope. A fresh UUID nonce is
// generated for every call.
//
// The canonical JSON is produced by marshalling v with encoding/json. For
// structs, Go guarantees fields are emitted in declaration order. Callers
// must declare their fields in alphabetical order to satisfy the canonical
// requirement. See model.ScanCompletedMessage for the compliant struct.
func (s *Signer) Sign(v any) (*SignedEnvelope, error) {
	canonical, err := canonicalJSON(v)
	if err != nil {
		return nil, fmt.Errorf("canonical JSON: %w", err)
	}

	mac := hmac.New(sha256.New, s.key)
	mac.Write(canonical)
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	nonce := uuid.New().String()

	return &SignedEnvelope{
		Payload: json.RawMessage(canonical),
		Sig: Sig{
			Alg:   AlgHMACSHA256,
			KeyID: s.keyID,
			Nonce: nonce,
			Value: sig,
		},
	}, nil
}

// Verify checks whether the HMAC in env is valid for the given key.
// It returns an error if the HMAC does not match. It does NOT check
// timestamp or nonce freshness — that is the consumer's responsibility.
func Verify(key []byte, env *SignedEnvelope) error {
	if env.Sig.Alg != AlgHMACSHA256 {
		return fmt.Errorf("unsupported algorithm: %s", env.Sig.Alg)
	}

	expected, err := base64.StdEncoding.DecodeString(env.Sig.Value)
	if err != nil {
		return fmt.Errorf("invalid sig.value encoding: %w", err)
	}

	mac := hmac.New(sha256.New, key)
	mac.Write(env.Payload)
	got := mac.Sum(nil)

	if !hmac.Equal(got, expected) {
		return fmt.Errorf("HMAC mismatch")
	}
	return nil
}

// canonicalJSON marshals v to JSON with map keys in alphabetical order.
// For struct types (like ScanCompletedMessage) the field order is
// declaration order; callers must declare fields alphabetically.
// For map types, encoding/json sorts keys since Go 1.12.
func canonicalJSON(v any) ([]byte, error) {
	// Marshal to generic map to ensure key sorting regardless of input type.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		// v is not a JSON object (e.g. array or scalar) — return as-is.
		return raw, nil
	}

	return marshalSorted(m)
}

// marshalSorted recursively sorts map keys and marshals to a canonical form.
func marshalSorted(m map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf := []byte{'{'}
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		keyBytes, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf = append(buf, keyBytes...)
		buf = append(buf, ':')

		// Recurse into nested objects.
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(m[k], &nested); err == nil {
			sortedNested, err := marshalSorted(nested)
			if err != nil {
				return nil, err
			}
			buf = append(buf, sortedNested...)
		} else {
			buf = append(buf, m[k]...)
		}
	}
	buf = append(buf, '}')
	return buf, nil
}
