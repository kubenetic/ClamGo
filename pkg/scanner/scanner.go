// Package scanner implements the ClamGo scan pipeline. It orchestrates:
//   - Cancellation pre-check (Redis + in-memory set)
//   - Magic byte inspection (actual MIME type detection)
//   - SHA-256 checksum computation
//   - ClamAV scan via clamd
//   - Post-scan cancellation check
//   - Result publication (file.scan.completed)
//   - Retry routing (file.scan.retrying → retry queues; file.scan.failed → DLQ)
package scanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"ClamGo/pkg/model"
	"ClamGo/pkg/service/clamd"

	"github.com/gabriel-vasile/mimetype"
	mqmodel "github.com/kubenetic/BunnyShepherd/pkg/model"
	rmq "github.com/kubenetic/BunnyShepherd/pkg/rabbitmq"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

const (
	headerRetryCount     = "x-retry-count"
	headerFirstFailureAt = "x-first-failure-at"
	headerLastError      = "x-last-error"
	maxRetries           = 3
)

// retryQueueNames maps retry attempt number (1-3) to the queue name.
var retryQueueNames = map[int]string{
	1: "q.file.scan.retry-1",
	2: "q.file.scan.retry-2",
	3: "q.file.scan.retry-3",
}

// retryDelayMs maps retry attempt number (1-3) to the TTL delay in ms.
var retryDelayMs = map[int]int64{
	1: 30_000,
	2: 120_000,
	3: 600_000,
}

// Config holds all configuration needed by the Scanner.
type Config struct {
	// TempNFSPrefix is the required filesystem prefix for scan file paths.
	// ClamGo rejects paths that don't start with this value.
	TempNFSPrefix string

	// Exchange is the main uploader exchange (uploader.exchange).
	Exchange string

	// DLX is the dead-letter exchange (uploader.dlx).
	DLX string

	// ScanCompletedRoutingKey is the routing key for completed scan results.
	ScanCompletedRoutingKey string

	// ScanRetryingRoutingKey is the routing key for retry notifications.
	ScanRetryingRoutingKey string

	// DLQRoutingKey is the routing key for the DLQ (file.scan.failed).
	DLQRoutingKey string

	// ClamdTCPAddr is the clamd TCP address (e.g. "localhost:3310").
	// If empty, unix socket is used.
	ClamdTCPAddr string

	// ClamdUnixPath is the path to the clamd Unix socket.
	ClamdUnixPath string
}

// Scanner is the main scan orchestrator. It is safe for concurrent use by
// the case-cancelled consumer goroutine (to update the cancelled set) and
// the scan consumer goroutine (to process messages). All shared state is
// protected by the cancelMu mutex.
type Scanner struct {
	cfg       Config
	pub       *rmq.Publisher
	redis     redis.UniversalClient
	cancelMu  sync.RWMutex
	cancelled map[string]struct{} // in-memory cancelled caseId set
}

// New creates a Scanner. The publisher must already be initialized.
func New(cfg Config, pub *rmq.Publisher, redisClient redis.UniversalClient) *Scanner {
	return &Scanner{
		cfg:       cfg,
		pub:       pub,
		redis:     redisClient,
		cancelled: make(map[string]struct{}),
	}
}

// MarkCancelled adds caseId to the in-memory cancelled set.
// Called by the case.cancelled consumer goroutine.
func (s *Scanner) MarkCancelled(caseId string) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	s.cancelled[caseId] = struct{}{}
	log.Info().Str("caseId", caseId).Msg("case marked as cancelled in memory")
}

// isCancelled returns true if the case has been cancelled, checking both the
// in-memory set and Redis for the cancelled:{caseId} key.
func (s *Scanner) isCancelled(ctx context.Context, caseId string) bool {
	s.cancelMu.RLock()
	_, inMem := s.cancelled[caseId]
	s.cancelMu.RUnlock()

	if inMem {
		return true
	}

	// Check Redis as a second-level fast-check.
	if s.redis != nil {
		key := fmt.Sprintf("cancelled:%s", caseId)
		val, err := s.redis.Exists(ctx, key).Result()
		if err == nil && val > 0 {
			// Cache locally to avoid future Redis hits.
			s.cancelMu.Lock()
			s.cancelled[caseId] = struct{}{}
			s.cancelMu.Unlock()
			return true
		}
	}

	return false
}

// HandleScanMessage is the BunnyShepherd MessageHandler for file.uploaded messages.
// It runs the full scan pipeline and is responsible for ACKing/NACKing the delivery.
// The BunnyShepherd consumer wraps this in a panic-recovery + auto-Nack fallback,
// but we manage ACK/Nack explicitly here for complete control over retry routing.
func (s *Scanner) HandleScanMessage(ctx context.Context, d amqp.Delivery) error {
	var msg model.FileUploadedMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		log.Error().Err(err).Msg("failed to unmarshal FileUploadedMessage; discarding (ACK)")
		_ = d.Ack(false)
		return nil
	}

	log := log.With().
		Str("fileId", msg.FileId).
		Str("caseId", msg.CaseId).
		Logger()

	// Pre-check: is the case cancelled?
	if s.isCancelled(ctx, msg.CaseId) {
		log.Info().Msg("case is cancelled; discarding message (ACK without scan)")
		_ = d.Ack(false)
		return nil
	}

	// Validate temp path prefix to prevent path traversal.
	if s.cfg.TempNFSPrefix != "" && !strings.HasPrefix(msg.TempPath, s.cfg.TempNFSPrefix) {
		log.Error().
			Str("tempPath", msg.TempPath).
			Str("requiredPrefix", s.cfg.TempNFSPrefix).
			Msg("temp path does not match required prefix; discarding (ACK)")
		_ = d.Ack(false)
		return nil
	}

	// Read retry count from AMQP headers.
	retryCount := extractRetryCount(d.Headers)

	// Run the scan.
	err := s.runScan(ctx, d, msg, retryCount)
	if err != nil {
		// runScan handles ACK/Nack and retry publishing internally.
		// Only return error here for BunnyShepherd panic-recovery path
		// (should not normally reach here).
		return err
	}

	return nil
}

// HandleCancelMessage is the BunnyShepherd MessageHandler for case.cancelled messages.
func (s *Scanner) HandleCancelMessage(ctx context.Context, d amqp.Delivery) error {
	var msg model.CaseCancelledMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		log.Error().Err(err).Msg("failed to unmarshal CaseCancelledMessage; discarding (ACK)")
		_ = d.Ack(false)
		return nil
	}

	s.MarkCancelled(msg.CaseId)
	_ = d.Ack(false)
	return nil
}

// runScan performs the full scan pipeline for a single file.
// It is responsible for ACKing the original delivery in ALL code paths.
func (s *Scanner) runScan(ctx context.Context, d amqp.Delivery, msg model.FileUploadedMessage, retryCount int) error {
	start := time.Now()
	log := log.With().
		Str("fileId", msg.FileId).
		Str("caseId", msg.CaseId).
		Int("retryCount", retryCount).
		Logger()

	// Open the file.
	f, err := os.Open(msg.TempPath)
	if err != nil {
		log.Error().Err(err).Str("tempPath", msg.TempPath).Msg("failed to open file for scanning")
		return s.handleScanFailure(ctx, d, msg, retryCount, "FILE_NOT_FOUND", err.Error(), d.Headers)
	}
	defer f.Close()

	// Compute SHA-256 and magic bytes in one pass.
	sha256hex, magicAnalysis, mimeErr := computeChecksumAndMagicBytes(f, msg.OriginalName, msg.ContentType)
	if mimeErr != nil {
		log.Warn().Err(mimeErr).Msg("magic byte detection failed; proceeding with UNKNOWN consistency")
	}

	// Rewind file for clamd scan.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		log.Error().Err(err).Msg("failed to rewind file for clamd scan")
		return s.handleScanFailure(ctx, d, msg, retryCount, "SCAN_ERROR", err.Error(), d.Headers)
	}
	f.Close() // clamd reads by path; close before passing path

	// Post-rewind cancellation check.
	if s.isCancelled(ctx, msg.CaseId) {
		log.Info().Msg("case cancelled during file read; discarding scan job (ACK)")
		_ = d.Ack(false)
		return nil
	}

	// Connect to clamd and scan.
	clamClient, err := s.newClamdClient()
	if err != nil {
		log.Error().Err(err).Msg("failed to connect to clamd")
		return s.handleScanFailure(ctx, d, msg, retryCount, "CLAMD_UNAVAILABLE", err.Error(), d.Headers)
	}
	defer clamClient.Close()

	// Get engine and signature versions before scan (best effort).
	engineVer, sigVer := s.getClamdVersions(clamClient)

	finding, err := clamClient.ScanFile(msg.TempPath)
	scanDuration := time.Since(start)
	if err != nil {
		if err == clamd.ErrFileNotFound {
			log.Error().Err(err).Str("tempPath", msg.TempPath).Msg("clamd could not find file")
			return s.handleScanFailure(ctx, d, msg, retryCount, "FILE_NOT_FOUND", err.Error(), d.Headers)
		}
		log.Error().Err(err).Msg("clamd scan error")
		return s.handleScanFailure(ctx, d, msg, retryCount, "SCAN_ERROR", err.Error(), d.Headers)
	}

	// Post-scan cancellation check: discard result if cancelled during scan.
	if s.isCancelled(ctx, msg.CaseId) {
		log.Info().Msg("case cancelled during scan; discarding result, deleting temp file")
		_ = os.Remove(msg.TempPath)
		_ = d.Ack(false)
		return nil
	}

	// Build and publish the scan completed message.
	var verdict model.Verdict
	var threatName string

	if finding == "OK" || finding == "" {
		verdict = model.VerdictClean
	} else {
		verdict = model.VerdictInfected
		threatName = finding
		// Delete infected file immediately.
		if removeErr := os.Remove(msg.TempPath); removeErr != nil {
			log.Warn().Err(removeErr).Str("tempPath", msg.TempPath).Msg("failed to delete infected file")
		}
	}

	result := model.ScanCompletedMessage{
		FileId:            msg.FileId,
		CaseId:            msg.CaseId,
		Verdict:           verdict,
		ThreatName:        threatName,
		ChecksumSha256:    sha256hex,
		MagicByteAnalysis: magicAnalysis,
		EngineVersion:     engineVer,
		SignatureVersion:  sigVer,
		ScannedAt:         time.Now().UTC(),
		ScanDurationMs:    scanDuration.Milliseconds(),
	}

	envelope := &mqmodel.JSONMessage[model.ScanCompletedMessage]{Payload: result}
	if err := s.pub.Publish(ctx, s.cfg.Exchange, s.cfg.ScanCompletedRoutingKey, false, envelope); err != nil {
		log.Error().Err(err).Msg("failed to publish ScanCompletedMessage; NACKing original")
		_ = d.Nack(false, false)
		return err
	}

	log.Info().
		Str("verdict", string(verdict)).
		Str("sha256", sha256hex).
		Int64("durationMs", scanDuration.Milliseconds()).
		Msg("scan completed, result published")

	_ = d.Ack(false)
	return nil
}

// handleScanFailure implements the retry routing logic (ACK + publish to retry queue or DLX).
func (s *Scanner) handleScanFailure(
	ctx context.Context,
	d amqp.Delivery,
	msg model.FileUploadedMessage,
	retryCount int,
	errorCode string,
	errorMsg string,
	origHeaders amqp.Table,
) error {
	nextRetry := retryCount + 1

	if retryCount < maxRetries {
		// Route to retry queue.
		queueName := retryQueueNames[nextRetry]
		delayMs := retryDelayMs[nextRetry]

		// Build headers for the retry message.
		firstFailureAt := extractStringHeader(origHeaders, headerFirstFailureAt)
		if firstFailureAt == "" {
			firstFailureAt = time.Now().UTC().Format(time.RFC3339)
		}

		retryHeaders := amqp.Table{
			headerRetryCount:     int64(nextRetry),
			headerFirstFailureAt: firstFailureAt,
			headerLastError:      errorCode,
		}

		// Publish a new file.uploaded message to the retry queue directly
		// (not via exchange — goes directly into the TTL queue).
		retryEnvelope := &mqmodel.JSONMessage[model.FileUploadedMessage]{
			Payload: msg,
			Headers: retryHeaders,
		}

		pubCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		if err := s.pub.Publish(pubCtx, "", queueName, false, retryEnvelope); err != nil {
			log.Error().Err(err).
				Str("fileId", msg.FileId).
				Str("queueName", queueName).
				Msg("failed to publish retry message; NACKing")
			_ = d.Nack(false, false)
			return err
		}

		// Publish file.scan.retrying notification for the Java Backend.
		retryNotif := model.ScanRetryingMessage{
			FileId:           msg.FileId,
			CaseId:           msg.CaseId,
			RetryAttempt:     nextRetry,
			MaxRetries:       maxRetries,
			Error:            errorCode,
			Message:          errorMsg,
			NextRetryQueue:   queueName,
			NextRetryDelayMs: delayMs,
			FailedAt:         time.Now().UTC(),
		}
		notifEnvelope := &mqmodel.JSONMessage[model.ScanRetryingMessage]{Payload: retryNotif}
		// Best effort: log but don't fail if this publish doesn't confirm.
		if err := s.pub.Publish(pubCtx, s.cfg.Exchange, s.cfg.ScanRetryingRoutingKey, false, notifEnvelope); err != nil {
			log.Warn().Err(err).Str("fileId", msg.FileId).Msg("failed to publish ScanRetryingMessage (non-fatal)")
		}

		log.Warn().
			Str("fileId", msg.FileId).
			Str("caseId", msg.CaseId).
			Str("error", errorCode).
			Int("nextRetry", nextRetry).
			Int("maxRetries", maxRetries).
			Str("retryQueue", queueName).
			Msgf("scan failed, scheduling retry %d/%d", nextRetry, maxRetries)

		_ = d.Ack(false)
		return nil
	}

	// Retries exhausted — publish to DLX.
	failedMsg := model.ScanFailedMessage{
		FileId:          msg.FileId,
		CaseId:          msg.CaseId,
		Error:           errorCode,
		Message:         errorMsg,
		RetryCount:      retryCount,
		OriginalMessage: msg,
		FailedAt:        time.Now().UTC(),
	}

	dlxEnvelope := &mqmodel.JSONMessage[model.ScanFailedMessage]{Payload: failedMsg}
	pubCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := s.pub.Publish(pubCtx, s.cfg.DLX, s.cfg.DLQRoutingKey, false, dlxEnvelope); err != nil {
		log.Error().Err(err).
			Str("fileId", msg.FileId).
			Msg("failed to publish ScanFailedMessage to DLX; NACKing")
		_ = d.Nack(false, false)
		return err
	}

	log.Error().
		Str("fileId", msg.FileId).
		Str("caseId", msg.CaseId).
		Str("error", errorCode).
		Msgf("scan permanently failed after %d retries for file %s", retryCount, msg.FileId)

	_ = d.Ack(false)
	return nil
}

// newClamdClient creates a new ClamClient connection using the configured protocol.
func (s *Scanner) newClamdClient() (*clamd.ClamClient, error) {
	c := &clamd.ClamClient{}
	if s.cfg.ClamdTCPAddr != "" {
		if err := c.Connect("tcp", s.cfg.ClamdTCPAddr); err != nil {
			return nil, fmt.Errorf("connect clamd tcp %s: %w", s.cfg.ClamdTCPAddr, err)
		}
		return c, nil
	}
	if s.cfg.ClamdUnixPath != "" {
		if err := c.Connect("unix", s.cfg.ClamdUnixPath); err != nil {
			return nil, fmt.Errorf("connect clamd unix %s: %w", s.cfg.ClamdUnixPath, err)
		}
		return c, nil
	}
	return nil, fmt.Errorf("no clamd connection configured (set clamd.tcp.addr or clamd.unix.path)")
}

// getClamdVersions returns the engine and database signature version strings
// from clamd's VERSION response. Returns empty strings on error.
// Example VERSION response: "ClamAV 1.4.1/27450/Wed Feb 26 08:15:00 2026"
func (s *Scanner) getClamdVersions(c *clamd.ClamClient) (engineVer, sigVer string) {
	versionBytes, err := c.Version()
	if err != nil {
		return "", ""
	}

	// Parse "ClamAV X.Y.Z/NNNNN/..."
	versionStr := strings.TrimSpace(string(versionBytes))
	re := regexp.MustCompile(`ClamAV\s+([^\s/]+)/(\d+)`)
	m := re.FindStringSubmatch(versionStr)
	if len(m) == 3 {
		return m[1], m[2]
	}

	return versionStr, ""
}

// computeChecksumAndMagicBytes reads the file once, computing the SHA-256
// checksum and detecting the actual MIME type via magic bytes.
func computeChecksumAndMagicBytes(r io.Reader, originalName, claimedMimeType string) (string, model.MagicByteAnalysis, error) {
	// Read first 3KB for magic byte detection (mimetype reads header only).
	headerBuf := make([]byte, 3072)
	n, _ := io.ReadFull(r, headerBuf)
	headerBuf = headerBuf[:n]

	detectedMT := mimetype.Detect(headerBuf)
	detectedMime := ""
	if detectedMT != nil {
		detectedMime = detectedMT.String()
	}

	// Compute SHA-256 over header bytes already read + rest of file.
	h := sha256.New()
	h.Write(headerBuf)
	if _, err := io.Copy(h, r); err != nil {
		return "", model.MagicByteAnalysis{}, fmt.Errorf("sha256 computation: %w", err)
	}
	checksum := hex.EncodeToString(h.Sum(nil))

	// Determine claimed extension from original filename.
	claimedExt := strings.ToLower(filepath.Ext(originalName))

	// Classify consistency.
	analysis := model.MagicByteAnalysis{
		DetectedMimeType: detectedMime,
		ClaimedMimeType:  claimedMimeType,
		ClaimedExtension: claimedExt,
		Consistency:      classifyConsistency(detectedMime, claimedMimeType, claimedExt),
	}

	return checksum, analysis, nil
}

// classifyConsistency determines how well the detected MIME type matches the claimed one.
func classifyConsistency(detected, claimed, ext string) model.MagicByteConsistency {
	if detected == "" {
		return model.ConsistencyUnknown
	}
	if claimed == "" && ext == "" {
		return model.ConsistencyEmpty
	}

	detectedBase := mimeBase(detected)
	claimedBase := mimeBase(claimed)

	if detectedBase == claimedBase {
		return model.ConsistencyConsistent
	}

	// Check whether the detected type is a well-known alias for the claimed type.
	if mimeAlias(detected, claimed) || mimeAlias(claimed, detected) {
		return model.ConsistencyConsistent
	}

	// Same top-level type (e.g. both "application/*") but different subtype.
	if sameTopLevel(detected, claimed) {
		return model.ConsistencyMinorMismatch
	}

	return model.ConsistencyMismatch
}

func mimeBase(mime string) string {
	// Strip parameters (e.g. "; charset=utf-8")
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = mime[:i]
	}
	return strings.TrimSpace(strings.ToLower(mime))
}

func sameTopLevel(a, b string) bool {
	topA := strings.SplitN(mimeBase(a), "/", 2)[0]
	topB := strings.SplitN(mimeBase(b), "/", 2)[0]
	return topA == topB && topA != ""
}

// mimeAlias returns true when a and b are known aliases for the same format.
var knownAliases = [][2]string{
	{"application/zip", "application/x-zip-compressed"},
	{"application/zip", "application/x-zip"},
	{"application/pdf", "application/x-pdf"},
	{"application/msword", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	{"image/jpeg", "image/jpg"},
	{"text/plain", "text/x-log"},
}

func mimeAlias(a, b string) bool {
	a, b = mimeBase(a), mimeBase(b)
	for _, pair := range knownAliases {
		if (pair[0] == a && pair[1] == b) || (pair[0] == b && pair[1] == a) {
			return true
		}
	}
	return false
}

// extractRetryCount reads the x-retry-count integer header from AMQP headers.
// Returns 0 if the header is absent or cannot be parsed.
func extractRetryCount(headers amqp.Table) int {
	if headers == nil {
		return 0
	}
	v, ok := headers[headerRetryCount]
	if !ok {
		return 0
	}
	switch val := v.(type) {
	case int64:
		return int(val)
	case int32:
		return int(val)
	case int:
		return val
	case float64:
		return int(val)
	}
	return 0
}

// extractStringHeader reads a string header value from AMQP headers.
func extractStringHeader(headers amqp.Table, key string) string {
	if headers == nil {
		return ""
	}
	v, ok := headers[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
