package hmac

import (
	cryptohmac "crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKey is a fixed 32-byte key used for deterministic unit tests.
var testKey = []byte("12345678901234567890123456789012")

// samplePayload represents a typical scan result struct.
type samplePayload struct {
	CaseID    string    `json:"caseId"`
	FileID    string    `json:"fileId"`
	ScannedAt time.Time `json:"scannedAt"`
	Verdict   string    `json:"verdict"`
}

func TestNewSigner_RejectsShortKey(t *testing.T) {
	_, err := NewSigner([]byte("tooshort"), KeyIDV1)
	require.Error(t, err)
}

func TestNewSigner_RejectsEmptyKeyID(t *testing.T) {
	_, err := NewSigner(testKey, "")
	require.Error(t, err)
}

func TestSigner_SignAndVerify_RoundTrip(t *testing.T) {
	signer, err := NewSigner(testKey, KeyIDV1)
	require.NoError(t, err)

	payload := samplePayload{
		FileID:    "file-001",
		CaseID:    "case-001",
		Verdict:   "CLEAN",
		ScannedAt: time.Date(2026, 5, 3, 10, 12, 34, 0, time.UTC),
	}

	env, err := signer.Sign(payload)
	require.NoError(t, err)

	assert.Equal(t, AlgHMACSHA256, env.Sig.Alg)
	assert.Equal(t, KeyIDV1, env.Sig.KeyID)
	assert.NotEmpty(t, env.Sig.Nonce)
	assert.NotEmpty(t, env.Sig.Value)

	// Verify with the same key — should pass.
	err = Verify(testKey, env)
	assert.NoError(t, err)
}

func TestVerify_WrongKey(t *testing.T) {
	signer, err := NewSigner(testKey, KeyIDV1)
	require.NoError(t, err)

	payload := samplePayload{FileID: "f1", CaseID: "c1", Verdict: "CLEAN", ScannedAt: time.Now()}
	env, err := signer.Sign(payload)
	require.NoError(t, err)

	wrongKey := []byte("99999999999999999999999999999999")
	err = Verify(wrongKey, env)
	assert.Error(t, err, "wrong key must produce HMAC mismatch")
}

func TestVerify_TamperedPayload(t *testing.T) {
	signer, err := NewSigner(testKey, KeyIDV1)
	require.NoError(t, err)

	payload := samplePayload{FileID: "f1", CaseID: "c1", Verdict: "CLEAN", ScannedAt: time.Now()}
	env, err := signer.Sign(payload)
	require.NoError(t, err)

	// Tamper: replace "CLEAN" with "INFECTED" in the payload bytes.
	tampered := []byte(`{"caseId":"c1","fileId":"f1","scannedAt":"` + time.Now().Format(time.RFC3339) + `","verdict":"INFECTED"}`)
	env.Payload = json.RawMessage(tampered)

	err = Verify(testKey, env)
	assert.Error(t, err, "tampered payload must fail verification")
}

func TestVerify_UnsupportedAlgorithm(t *testing.T) {
	env := &SignedEnvelope{
		Payload: json.RawMessage(`{"verdict":"CLEAN"}`),
		Sig: Sig{
			Alg:   "HMAC-MD5",
			KeyID: KeyIDV1,
			Nonce: "nonce-123",
			Value: base64.StdEncoding.EncodeToString([]byte("bad")),
		},
	}
	err := Verify(testKey, env)
	assert.Error(t, err)
}

func TestSign_GeneratesUniqueNonces(t *testing.T) {
	signer, err := NewSigner(testKey, KeyIDV1)
	require.NoError(t, err)

	payload := samplePayload{FileID: "f1", CaseID: "c1", Verdict: "CLEAN", ScannedAt: time.Now()}

	env1, _ := signer.Sign(payload)
	env2, _ := signer.Sign(payload)

	assert.NotEqual(t, env1.Sig.Nonce, env2.Sig.Nonce, "each sign call must produce a unique nonce")
}

func TestCanonicalJSON_SortsKeys(t *testing.T) {
	input := map[string]any{
		"z": "last",
		"a": "first",
		"m": "middle",
	}

	out, err := canonicalJSON(input)
	require.NoError(t, err)

	// Keys must appear in alphabetical order.
	var result map[string]string
	require.NoError(t, json.Unmarshal(out, &result))
	assert.Equal(t, "first", result["a"])
	assert.Equal(t, "middle", result["m"])
	assert.Equal(t, "last", result["z"])

	// Raw JSON bytes must reflect alphabetical key order.
	raw := string(out)
	aIdx := findKeyIndex(raw, `"a"`)
	mIdx := findKeyIndex(raw, `"m"`)
	zIdx := findKeyIndex(raw, `"z"`)
	assert.Less(t, aIdx, mIdx, "a must come before m")
	assert.Less(t, mIdx, zIdx, "m must come before z")
}

func findKeyIndex(s, key string) int {
	for i := 0; i+len(key) <= len(s); i++ {
		if s[i:i+len(key)] == key {
			return i
		}
	}
	return -1
}

// ─── Known-Answer Test (C-1) ───────────────────────────────────────────────────
//
// This test asserts cross-language agreement with the Java backend.
// The key, payload, and expected signature are fixed constants that MUST match
// the Java-side verification logic exactly.
//
//	key_base64 = MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
//	             (decodes to ASCII "0123456789abcdef0123456789abcdef")
//
//	payload bytes (exact) = {"caseId":"c1","fileId":"f1","scannedAt":"2026-01-01T00:00:00Z","verdict":"CLEAN"}
//	expected sig.value (base64) = 4B/YIqyBDlkBLULhMrjqpwaGyQj2sptDU2xWkflVusQ=
func TestKnownAnswer_CrossLanguageHMAC(t *testing.T) {
	const (
		keyB64         = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
		payloadStr     = `{"caseId":"c1","fileId":"f1","scannedAt":"2026-01-01T00:00:00Z","verdict":"CLEAN"}`
		expectedSigB64 = "4B/YIqyBDlkBLULhMrjqpwaGyQj2sptDU2xWkflVusQ="
	)

	// Decode the key.
	keyBytes, err := base64.StdEncoding.DecodeString(keyB64)
	require.NoError(t, err, "key must be valid base64")
	require.Len(t, keyBytes, 32, "key must be 32 bytes")

	// Verify that the key decodes to the expected ASCII string.
	assert.Equal(t, "0123456789abcdef0123456789abcdef", string(keyBytes))

	// Build a SignedEnvelope with the exact payload bytes (no re-serialisation).
	env := &SignedEnvelope{
		Payload: json.RawMessage(payloadStr),
		Sig: Sig{
			Alg:   AlgHMACSHA256,
			KeyID: KeyIDV1,
			Nonce: "test-nonce",
			Value: expectedSigB64,
		},
	}

	// Verify() must accept this envelope.
	err = Verify(keyBytes, env)
	require.NoError(t, err, "Verify must accept the known-answer envelope")

	// Also assert that computing HMAC over the exact payload bytes yields the expected tag.
	// This is the same computation the Java backend performs.
	mac := cryptohmac.New(sha256.New, keyBytes)
	mac.Write([]byte(payloadStr))
	gotSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	assert.Equal(t, expectedSigB64, gotSig, "HMAC over exact payload bytes must match expected tag")
}
