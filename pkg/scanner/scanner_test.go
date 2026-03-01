// Package scanner provides tests for the ClamGo scan pipeline.
package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ClamGo/pkg/model"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── Magic Byte Detection Tests ────────────────────────────────────────────────

func TestClassifyConsistency(t *testing.T) {
	tests := []struct {
		name     string
		detected string
		claimed  string
		ext      string
		expected model.MagicByteConsistency
	}{
		{
			name:     "exact match",
			detected: "application/pdf",
			claimed:  "application/pdf",
			ext:      ".pdf",
			expected: model.ConsistencyConsistent,
		},
		{
			name:     "case insensitive match",
			detected: "APPLICATION/PDF",
			claimed:  "application/pdf",
			ext:      ".pdf",
			expected: model.ConsistencyConsistent,
		},
		{
			name:     "known alias - zip variants",
			detected: "application/zip",
			claimed:  "application/x-zip-compressed",
			ext:      ".zip",
			expected: model.ConsistencyConsistent,
		},
		{
			name:     "known alias - jpeg variants",
			detected: "image/jpeg",
			claimed:  "image/jpg",
			ext:      ".jpg",
			expected: model.ConsistencyConsistent,
		},
		{
			name:     "same top level type - minor mismatch",
			detected: "image/png",
			claimed:  "image/jpeg",
			ext:      ".jpg",
			expected: model.ConsistencyMinorMismatch,
		},
		{
			name:     "different top level - mismatch",
			detected: "application/pdf",
			claimed:  "image/jpeg",
			ext:      ".jpg",
			expected: model.ConsistencyMismatch,
		},
		{
			name:     "unknown detection",
			detected: "",
			claimed:  "application/pdf",
			ext:      ".pdf",
			expected: model.ConsistencyUnknown,
		},
		{
			name:     "empty claimed and extension",
			detected: "application/pdf",
			claimed:  "",
			ext:      "",
			expected: model.ConsistencyEmpty,
		},
		{
			name:     "mime with charset parameter",
			detected: "text/html; charset=utf-8",
			claimed:  "text/html",
			ext:      ".html",
			expected: model.ConsistencyConsistent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyConsistency(tt.detected, tt.claimed, tt.ext)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMimeBase(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"application/pdf", "application/pdf"},
		{"text/html; charset=utf-8", "text/html"},
		{"IMAGE/PNG", "image/png"},
		{"  application/json  ", "application/json"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := mimeBase(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSameTopLevel(t *testing.T) {
	tests := []struct {
		a, b     string
		expected bool
	}{
		{"image/png", "image/jpeg", true},
		{"application/pdf", "application/json", true},
		{"image/png", "application/pdf", false},
		{"", "image/png", false},
		{"image/png", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			result := sameTopLevel(tt.a, tt.b)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMimeAlias(t *testing.T) {
	tests := []struct {
		a, b     string
		expected bool
	}{
		{"application/zip", "application/x-zip-compressed", true},
		{"application/x-zip-compressed", "application/zip", true},
		{"image/jpeg", "image/jpg", true},
		{"application/pdf", "application/x-pdf", true},
		{"application/pdf", "image/jpeg", false},
		{"application/unknown", "application/other", false},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			result := mimeAlias(tt.a, tt.b)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ─── Retry Header Extraction Tests ─────────────────────────────────────────────

func TestExtractRetryCount(t *testing.T) {
	tests := []struct {
		name     string
		headers  amqp.Table
		expected int
	}{
		{
			name:     "nil headers",
			headers:  nil,
			expected: 0,
		},
		{
			name:     "empty headers",
			headers:  amqp.Table{},
			expected: 0,
		},
		{
			name:     "missing retry header",
			headers:  amqp.Table{"other-header": "value"},
			expected: 0,
		},
		{
			name:     "int64 value",
			headers:  amqp.Table{headerRetryCount: int64(2)},
			expected: 2,
		},
		{
			name:     "int32 value",
			headers:  amqp.Table{headerRetryCount: int32(1)},
			expected: 1,
		},
		{
			name:     "int value",
			headers:  amqp.Table{headerRetryCount: 3},
			expected: 3,
		},
		{
			name:     "float64 value",
			headers:  amqp.Table{headerRetryCount: float64(2.0)},
			expected: 2,
		},
		{
			name:     "string value (not parseable)",
			headers:  amqp.Table{headerRetryCount: "not-a-number"},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractRetryCount(tt.headers)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractStringHeader(t *testing.T) {
	tests := []struct {
		name     string
		headers  amqp.Table
		key      string
		expected string
	}{
		{
			name:     "nil headers",
			headers:  nil,
			key:      "test",
			expected: "",
		},
		{
			name:     "missing key",
			headers:  amqp.Table{"other": "value"},
			key:      "test",
			expected: "",
		},
		{
			name:     "string value",
			headers:  amqp.Table{"test": "hello"},
			key:      "test",
			expected: "hello",
		},
		{
			name:     "non-string value",
			headers:  amqp.Table{"test": 123},
			key:      "test",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractStringHeader(tt.headers, tt.key)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ─── Retry Queue Names Tests ────────────────────────────────────────────────────

func TestRetryQueueNames(t *testing.T) {
	assert.Equal(t, "q.file.scan.retry-1", retryQueueNames[1])
	assert.Equal(t, "q.file.scan.retry-2", retryQueueNames[2])
	assert.Equal(t, "q.file.scan.retry-3", retryQueueNames[3])
}

func TestRetryDelays(t *testing.T) {
	assert.Equal(t, int64(30_000), retryDelayMs[1], "retry 1 should be 30s")
	assert.Equal(t, int64(120_000), retryDelayMs[2], "retry 2 should be 2min")
	assert.Equal(t, int64(600_000), retryDelayMs[3], "retry 3 should be 10min")
}

// ─── Scanner Cancellation Tests ────────────────────────────────────────────────

func TestScanner_MarkCancelled(t *testing.T) {
	s := &Scanner{
		cancelled: make(map[string]struct{}),
	}

	// Initially not cancelled
	assert.False(t, s.isCancelledInMemory("case-123"))

	// Mark as cancelled
	s.MarkCancelled("case-123")

	// Now should be cancelled
	assert.True(t, s.isCancelledInMemory("case-123"))

	// Other cases should not be affected
	assert.False(t, s.isCancelledInMemory("case-456"))
}

// Helper to check in-memory cancellation without Redis
func (s *Scanner) isCancelledInMemory(caseId string) bool {
	s.cancelMu.RLock()
	defer s.cancelMu.RUnlock()
	_, exists := s.cancelled[caseId]
	return exists
}

func TestScanner_IsCancelled_WithRedis(t *testing.T) {
	// This test would require a mock Redis client
	// For now, we test the in-memory path only
	ctx := context.Background()
	s := &Scanner{
		cancelled: make(map[string]struct{}),
		redis:     nil, // No Redis client
	}

	// Not cancelled
	assert.False(t, s.isCancelled(ctx, "case-123"))

	// Mark cancelled
	s.MarkCancelled("case-123")

	// Now cancelled
	assert.True(t, s.isCancelled(ctx, "case-123"))
}

// ─── Handle Cancel Message Tests ───────────────────────────────────────────────

func TestScanner_HandleCancelMessage(t *testing.T) {
	s := &Scanner{
		cancelled: make(map[string]struct{}),
	}

	msg := model.CaseCancelledMessage{
		CaseId:      "test-case-id",
		CancelledBy: "user-123",
		CancelledAt: time.Now(),
		FileIds:     []string{"file-1", "file-2"},
	}

	body, err := json.Marshal(msg)
	require.NoError(t, err)

	// Create a mock delivery
	delivery := amqp.Delivery{
		Body:         body,
		Acknowledger: &mockAcknowledger{},
	}

	ctx := context.Background()
	err = s.HandleCancelMessage(ctx, delivery)
	assert.NoError(t, err)

	// Verify case was marked as cancelled
	assert.True(t, s.isCancelledInMemory("test-case-id"))
}

func TestScanner_HandleCancelMessage_InvalidJSON(t *testing.T) {
	s := &Scanner{
		cancelled: make(map[string]struct{}),
	}

	delivery := amqp.Delivery{
		Body:         []byte("invalid json"),
		Acknowledger: &mockAcknowledger{},
	}

	ctx := context.Background()
	err := s.HandleCancelMessage(ctx, delivery)
	// Should not return error - message is ACKed and discarded
	assert.NoError(t, err)
}

// ─── Handle Scan Message Tests ─────────────────────────────────────────────────

func TestScanner_HandleScanMessage_CancelledCase(t *testing.T) {
	s := &Scanner{
		cancelled: make(map[string]struct{}),
		cfg: Config{
			TempNFSPrefix: "/tmp/",
		},
	}

	// Mark case as cancelled before receiving scan message
	s.MarkCancelled("cancelled-case")

	msg := model.FileUploadedMessage{
		FileId:       "file-123",
		CaseId:       "cancelled-case",
		TempPath:     "/tmp/test-file.pdf",
		OriginalName: "test.pdf",
		SizeBytes:    1024,
		ContentType:  "application/pdf",
		UploadedAt:   time.Now(),
	}

	body, err := json.Marshal(msg)
	require.NoError(t, err)

	mockAck := &mockAcknowledger{}
	delivery := amqp.Delivery{
		Body:         body,
		Acknowledger: mockAck,
	}

	ctx := context.Background()
	err = s.HandleScanMessage(ctx, delivery)
	assert.NoError(t, err)

	// Message should be ACKed (discarded without scanning)
	assert.True(t, mockAck.acked)
}

func TestScanner_HandleScanMessage_InvalidTempPath(t *testing.T) {
	s := &Scanner{
		cancelled: make(map[string]struct{}),
		cfg: Config{
			TempNFSPrefix: "/mnt/temp-nfs/",
		},
	}

	msg := model.FileUploadedMessage{
		FileId:   "file-123",
		CaseId:   "case-456",
		TempPath: "/etc/passwd", // Invalid path - not in temp NFS
	}

	body, err := json.Marshal(msg)
	require.NoError(t, err)

	mockAck := &mockAcknowledger{}
	delivery := amqp.Delivery{
		Body:         body,
		Acknowledger: mockAck,
	}

	ctx := context.Background()
	err = s.HandleScanMessage(ctx, delivery)
	assert.NoError(t, err)

	// Message should be ACKed (discarded due to invalid path)
	assert.True(t, mockAck.acked)
}

func TestScanner_HandleScanMessage_InvalidJSON(t *testing.T) {
	s := &Scanner{
		cancelled: make(map[string]struct{}),
		cfg: Config{
			TempNFSPrefix: "/tmp/",
		},
	}

	mockAck := &mockAcknowledger{}
	delivery := amqp.Delivery{
		Body:         []byte("not valid json"),
		Acknowledger: mockAck,
	}

	ctx := context.Background()
	err := s.HandleScanMessage(ctx, delivery)
	assert.NoError(t, err)

	// Message should be ACKed (discarded due to parse error)
	assert.True(t, mockAck.acked)
}

// ─── Checksum and Magic Bytes Tests ────────────────────────────────────────────

func TestComputeChecksumAndMagicBytes(t *testing.T) {
	// Create a temporary test file
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "test.pdf")

	// Write PDF header + some content
	pdfContent := []byte("%PDF-1.4\n%âãÏÓ\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF")
	err := os.WriteFile(testFile, pdfContent, 0644)
	require.NoError(t, err)

	// Open and read
	f, err := os.Open(testFile)
	require.NoError(t, err)
	defer f.Close()

	checksum, analysis, err := computeChecksumAndMagicBytes(f, "test.pdf", "application/pdf")
	require.NoError(t, err)

	// Verify checksum is a valid hex string
	assert.Len(t, checksum, 64, "SHA-256 should be 64 hex characters")

	// Verify magic byte analysis
	assert.NotEmpty(t, analysis.DetectedMimeType)
	assert.Equal(t, "application/pdf", analysis.ClaimedMimeType)
	assert.Equal(t, ".pdf", analysis.ClaimedExtension)
}

func TestComputeChecksumAndMagicBytes_EmptyFile(t *testing.T) {
	// Create an empty file
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "empty.txt")
	err := os.WriteFile(testFile, []byte{}, 0644)
	require.NoError(t, err)

	f, err := os.Open(testFile)
	require.NoError(t, err)
	defer f.Close()

	checksum, analysis, err := computeChecksumAndMagicBytes(f, "empty.txt", "text/plain")
	require.NoError(t, err)

	// SHA-256 of empty file
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", checksum)
	assert.Equal(t, ".txt", analysis.ClaimedExtension)
}

func TestComputeChecksumAndMagicBytes_LargeFile(t *testing.T) {
	// Create a file larger than the header buffer (3KB)
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "large.bin")

	content := bytes.Repeat([]byte("x"), 10000) // 10KB
	err := os.WriteFile(testFile, content, 0644)
	require.NoError(t, err)

	f, err := os.Open(testFile)
	require.NoError(t, err)
	defer f.Close()

	checksum, _, err := computeChecksumAndMagicBytes(f, "large.bin", "application/octet-stream")
	require.NoError(t, err)

	// Verify checksum is valid
	assert.Len(t, checksum, 64)
}

// ─── Mock Types ────────────────────────────────────────────────────────────────

type mockAcknowledger struct {
	acked  bool
	nacked bool
}

func (m *mockAcknowledger) Ack(tag uint64, multiple bool) error {
	m.acked = true
	return nil
}

func (m *mockAcknowledger) Nack(tag uint64, multiple bool, requeue bool) error {
	m.nacked = true
	return nil
}

func (m *mockAcknowledger) Reject(tag uint64, requeue bool) error {
	m.nacked = true
	return nil
}

// ─── Scanner Config Validation Tests ───────────────────────────────────────────

func TestConfig_Defaults(t *testing.T) {
	cfg := Config{}

	// Verify defaults are empty (not nil)
	assert.Empty(t, cfg.TempNFSPrefix)
	assert.Empty(t, cfg.Exchange)
	assert.Empty(t, cfg.ClamdTCPAddr)
}

func TestScanner_New(t *testing.T) {
	cfg := Config{
		TempNFSPrefix:           "/mnt/temp-nfs/",
		Exchange:                "uploader.exchange",
		DLX:                     "uploader.dlx",
		ScanCompletedRoutingKey: "file.scan.completed",
		ScanRetryingRoutingKey:  "file.scan.retrying",
		DLQRoutingKey:           "file.scan.failed",
		ClamdTCPAddr:            "localhost:3310",
	}

	// Test without Redis
	s := New(cfg, nil, nil)

	assert.NotNil(t, s)
	assert.Equal(t, cfg.TempNFSPrefix, s.cfg.TempNFSPrefix)
	assert.Equal(t, cfg.Exchange, s.cfg.Exchange)
	assert.NotNil(t, s.cancelled)
}

// ─── Retry Queue Routing Tests ─────────────────────────────────────────────────

func TestRetryRouting_MaxRetriesConstant(t *testing.T) {
	assert.Equal(t, 3, maxRetries, "maxRetries should be 3")
}

func TestRetryRouting_QueueNames(t *testing.T) {
	// Verify all 3 retry queues are defined
	for i := 1; i <= maxRetries; i++ {
		queueName, ok := retryQueueNames[i]
		assert.True(t, ok, "retry queue %d should be defined", i)
		assert.NotEmpty(t, queueName)
	}
}

func TestRetryRouting_Delays(t *testing.T) {
	// Verify delays increase exponentially
	assert.Less(t, retryDelayMs[1], retryDelayMs[2])
	assert.Less(t, retryDelayMs[2], retryDelayMs[3])
}
