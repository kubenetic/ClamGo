package hmac

import (
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
