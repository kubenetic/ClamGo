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
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
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

// publishCtx returns a context that inherits values from parent but whose
// cancellation is independent, with the given timeout. This guarantees that
// a publish performed after a long clamd scan is not instantly cancelled
// because the message-handler context already expired.
func publishCtx(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

const (
	headerRetryCount     = "x-retry-count"
	headerFirstFailureAt = "x-first-failure-at"
	headerLastError      = "x-last-error"
	maxRetries           = 3

	// cancelledTTL is how long a cancelled caseId is retained in the in-memory set.
	// Entries older than this are evicted by the background cleanup goroutine.
	// Matches the Redis TTL that was previously used for the cancelled:{caseId} key.
	cancelledTTL = 24 * time.Hour

	// cancelledCleanupInterval is how often the cleanup goroutine runs.
	cancelledCleanupInterval = 30 * time.Minute

	// maxCancelledEntries is the maximum number of entries in the cancelled map.
	// Prevents unbounded memory growth from a flood of cancellation messages.
	maxCancelledEntries = 100_000
)

// clamVersionRe parses clamd's VERSION response.
// Example: "ClamAV 1.4.1/27450/Wed Feb 26 08:15:00 2026"
var clamVersionRe = regexp.MustCompile(`ClamAV\s+([^\s/]+)/(\d+)`)

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

// validateTempPath validates that path is safe for scanning.
// It checks for control characters, path traversal, and ensures the path
// starts with the required prefix.
func validateTempPath(path, prefix string) error {
	if path == "" {
		return fmt.Errorf("temp path is empty")
	}

	if prefix == "" {
		return fmt.Errorf("temp path prefix is empty")
	}

	if strings.ContainsAny(path, "\r\n\x00") {
		return fmt.Errorf("temp path contains control characters")
	}

	cleaned := filepath.Clean(path)

	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("temp path must be absolute: %s", cleaned)
	}

	// Normalize prefix to end with separator
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}

	if !strings.HasPrefix(cleaned+string(filepath.Separator), prefix) {
		return fmt.Errorf("temp path does not start with required prefix %s: %s", prefix, cleaned)
	}

	return nil
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

	// ScanStartedRoutingKey is the routing key for scan started notifications.
	ScanStartedRoutingKey string

	// ClamdTCPAddr is the clamd TCP address (e.g. "localhost:3310").
	// If empty, unix socket is used.
	ClamdTCPAddr string

	// ClamdUnixPath is the path to the clamd Unix socket.
	ClamdUnixPath string

	// MaxFileSizeBytes is the maximum allowed file size for scanning.
	// Files larger than this are rejected with VerdictError.
	MaxFileSizeBytes int64

	// StaleFilesLogDir is the directory where orphaned files are logged for manual cleanup.
	// If empty, defaults to /var/lib/clamgo.
	StaleFilesLogDir string
}

// cancelledEntry records when a caseId was marked as cancelled.
type cancelledEntry struct {
	at time.Time
}

// Scanner is the main scan orchestrator. It is safe for concurrent use by
// the case-cancelled consumer goroutine (to update the cancelled set) and
// the scan consumer goroutine (to process messages). All shared state is
// protected by the cancelMu mutex.
type Scanner struct {
	cfg              Config
	pub              *rmq.Publisher
	redis            redis.UniversalClient
	cancelMu         sync.RWMutex
	cancelledCurrent map[string]cancelledEntry // current generation of cancelled caseIds
	cancelledPrev    map[string]cancelledEntry // previous generation (for LRU eviction)
}

// New creates a Scanner. The publisher must already be initialized.
// A background goroutine is started to evict stale cancelled entries; it stops
// when ctx is cancelled.
func New(cfg Config, pub *rmq.Publisher, redisClient redis.UniversalClient) *Scanner {
	s := &Scanner{
		cfg:              cfg,
		pub:              pub,
		redis:            redisClient,
		cancelledCurrent: make(map[string]cancelledEntry),
		cancelledPrev:    make(map[string]cancelledEntry),
	}
	return s
}

// StartCleanup starts the background goroutine that evicts cancelled entries
// older than cancelledTTL. It runs until ctx is cancelled.
// Call this after New() in main, passing the application context.
func (s *Scanner) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(cancelledCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.evictStaleCancelled()
			}
		}
	}()
}

// evictStaleCancelled removes cancelled entries older than cancelledTTL.
func (s *Scanner) evictStaleCancelled() {
	cutoff := time.Now().Add(-cancelledTTL)
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	evicted := 0
	for id, entry := range s.cancelledCurrent {
		if entry.at.Before(cutoff) {
			delete(s.cancelledCurrent, id)
			evicted++
		}
	}
	for id, entry := range s.cancelledPrev {
		if entry.at.Before(cutoff) {
			delete(s.cancelledPrev, id)
			evicted++
		}
	}
	if evicted > 0 {
		log.Debug().Int("count", evicted).Msg("evicted stale cancelled entries")
	}
}

// MarkCancelled adds caseId to the in-memory cancelled set.
// Called by the case.cancelled consumer goroutine.
// Uses a two-generation LRU approach: when the current generation reaches capacity,
// it is swapped with the previous generation (evicting the oldest entries).
func (s *Scanner) MarkCancelled(caseId string) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()

	// If already in current generation, update timestamp and return.
	if _, exists := s.cancelledCurrent[caseId]; exists {
		s.cancelledCurrent[caseId] = cancelledEntry{at: time.Now()}
		return
	}

	// If current generation is at capacity, swap generations (evict oldest).
	if len(s.cancelledCurrent) >= maxCancelledEntries {
		log.Warn().
			Int("currentSize", len(s.cancelledCurrent)).
			Int("prevSize", len(s.cancelledPrev)).
			Msg("cancelled entries current generation at capacity; swapping generations")
		s.cancelledPrev = s.cancelledCurrent
		s.cancelledCurrent = make(map[string]cancelledEntry)
	}

	s.cancelledCurrent[caseId] = cancelledEntry{at: time.Now()}
	log.Info().Str("caseId", caseId).Msg("case marked as cancelled in memory")
}

// isCancelled returns true if the case has been cancelled, checking both the
// in-memory set (current and previous generations) and Redis for the cancelled:{caseId} key.
func (s *Scanner) isCancelled(ctx context.Context, caseId string) bool {
	s.cancelMu.RLock()
	entry, inMemCurrent := s.cancelledCurrent[caseId]
	_, inMemPrev := s.cancelledPrev[caseId]
	s.cancelMu.RUnlock()

	if inMemCurrent && time.Since(entry.at) < cancelledTTL {
		return true
	}

	if inMemPrev {
		// Entry is in previous generation; still valid if within TTL.
		// Promote it to current generation for faster future lookups.
		s.cancelMu.Lock()
		if prevEntry, exists := s.cancelledPrev[caseId]; exists && time.Since(prevEntry.at) < cancelledTTL {
			s.cancelledCurrent[caseId] = cancelledEntry{at: time.Now()}
			s.cancelMu.Unlock()
			return true
		}
		s.cancelMu.Unlock()
	}

	// Check Redis as a second-level fast-check with a short timeout.
	if s.redis != nil {
		redisCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		key := fmt.Sprintf("cancelled:%s", caseId)
		val, err := s.redis.Exists(redisCtx, key).Result()
		if err == nil && val > 0 {
			// Cache locally to avoid future Redis hits.
			s.cancelMu.Lock()
			s.cancelledCurrent[caseId] = cancelledEntry{at: time.Now()}
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
		// Unmarshal errors indicate malformed messages that cannot be recovered by retry.
		// ACK to discard and prevent requeue loops.
		log.Error().Err(err).Msg("failed to unmarshal FileUploadedMessage; discarding (ACK)")
		_ = d.Ack(false)
		return nil
	}

	// Normalize IDs to standard 36-char UUID format. Old tusd-token-hook versions
	// sent tusd's upload.ID (32-char hex without hyphens) as fileId; messages
	// already in the queue or in retry queues may still carry that format.
	msg.FileId = model.NormalizeUUID(msg.FileId)
	msg.CaseId = model.NormalizeUUID(msg.CaseId)

	log := log.With().
		Str("fileId", msg.FileId).
		Str("caseId", msg.CaseId).
		Logger()

	// Pre-check: is the case cancelled?
	if s.isCancelled(ctx, msg.CaseId) {
		log.Info().Msg("case is cancelled; discarding message (ACK without scan)")
		ackLogErr(d)
		return nil
	}

	// Validate temp path prefix to prevent path traversal.
	if err := validateTempPath(msg.TempPath, s.cfg.TempNFSPrefix); err != nil {
		log.Error().Err(err).Str("tempPath", msg.TempPath).Msg("temp path validation failed; discarding (ACK)")
		ackLogErr(d)
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
		ackLogErr(d)
		return nil
	}

	s.MarkCancelled(msg.CaseId)
	ackLogErr(d)
	return nil
}

// removeFileWithRetry attempts to remove a file, retrying once after 1 second if the first attempt fails.
// If both attempts fail, the path is appended to the stale-files log for manual cleanup.
// Returns true if the file was successfully removed or doesn't exist, false if it remains orphaned.
func (s *Scanner) removeFileWithRetry(filePath string) bool {
	// First attempt
	err := os.Remove(filePath)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return true
	}

	// Log the first failure
	log.Warn().Err(err).Str("tempPath", filePath).Msg("failed to delete file; retrying after 1 second")

	// Retry after 1 second
	time.Sleep(1 * time.Second)
	err = os.Remove(filePath)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return true
	}

	// Both attempts failed; log to stale-files ledger
	log.Warn().Err(err).Str("tempPath", filePath).Msg("failed to delete file after retry; logging to stale-files ledger")
	s.logOrphanFile(filePath)
	return false
}

// logOrphanFile appends the file path to the stale-files log for manual cleanup.
// Creates the log file if it doesn't exist.
func (s *Scanner) logOrphanFile(filePath string) {
	logDir := s.cfg.StaleFilesLogDir
	if logDir == "" {
		logDir = "/var/lib/clamgo"
	}
	logFile := filepath.Join(logDir, "stale-files.log")

	// Ensure the directory exists
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Error().Err(err).Str("dir", logDir).Msg("failed to create stale-files log directory")
		return
	}

	// Append the path to the log file
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Error().Err(err).Str("logFile", logFile).Msg("failed to open stale-files log")
		return
	}
	defer f.Close()

	timestamp := time.Now().UTC().Format(time.RFC3339)
	entry := fmt.Sprintf("%s %s\n", timestamp, filePath)
	if _, err := f.WriteString(entry); err != nil {
		log.Error().Err(err).Str("logFile", logFile).Msg("failed to write to stale-files log")
		return
	}

	log.Info().Str("logFile", logFile).Str("filePath", filePath).Msg("orphan file logged for cleanup")
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

	// Check for symlinks before opening the file (security: prevent arbitrary-file-read/delete).
	// This is a fast-path rejection, but the real protection is O_NOFOLLOW on open.
	fi, err := os.Lstat(msg.TempPath)
	if err != nil {
		log.Error().Err(err).Str("tempPath", msg.TempPath).Msg("file not found or unreadable; discarding (ACK)")
		ackLogErr(d)
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		log.Warn().Str("tempPath", msg.TempPath).Msg("symlink detected; rejecting for security (ACK without scan)")
		// Notify the uploader so the file is marked ERROR instead of stuck
		// in SCANNING forever. Best-effort — if the rejection publish fails,
		// we still ACK (the orphan-sweep job is the fallback).
		_ = s.publishRejection(ctx, msg, "SYMLINK_REJECTED")
		ackLogErr(d)
		return nil
	}

	// Check file size limit from message metadata (fast path).
	if s.cfg.MaxFileSizeBytes > 0 && msg.SizeBytes > s.cfg.MaxFileSizeBytes {
		log.Error().
			Int64("sizeBytes", msg.SizeBytes).
			Int64("maxFileSizeBytes", s.cfg.MaxFileSizeBytes).
			Msg("file exceeds maximum size; discarding (ACK)")
		// Notify the uploader so the file is marked ERROR rather than hanging
		// in SCANNING. Best-effort: if the publish fails the orphan-sweep
		// job will eventually reap the file, but the user will see no
		// immediate feedback.
		_ = s.publishRejection(ctx, msg, "FILE_TOO_LARGE")
		ackLogErr(d)
		return nil
	}

	// Open the file with O_NOFOLLOW to prevent symlink attacks (TOCTOU mitigation).
	f, err := os.OpenFile(msg.TempPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// If the error is ELOOP, it means the path is a symlink — report as security violation.
		if errors.Is(err, syscall.ELOOP) {
			log.Warn().Err(err).Str("tempPath", msg.TempPath).Msg("symlink detected at open time; rejecting for security (ACK)")
			_ = s.publishRejection(ctx, msg, "SYMLINK_REJECTED")
			ackLogErr(d)
			return nil
		}
		log.Error().Err(err).Str("tempPath", msg.TempPath).Msg("failed to open file; discarding (ACK)")
		ackLogErr(d)
		return nil
	}

	// Verify actual file size from the opened file descriptor (don't trust msg.SizeBytes from client).
	fi, err = f.Stat()
	if err != nil {
		f.Close()
		log.Error().Err(err).Str("tempPath", msg.TempPath).Msg("failed to stat opened file; discarding (ACK)")
		ackLogErr(d)
		return nil
	}
	if s.cfg.MaxFileSizeBytes > 0 && fi.Size() > s.cfg.MaxFileSizeBytes {
		f.Close()
		log.Error().
			Int64("actualSize", fi.Size()).
			Int64("maxFileSizeBytes", s.cfg.MaxFileSizeBytes).
			Msg("file exceeds maximum size on stat; discarding (ACK)")
		_ = s.publishRejection(ctx, msg, "FILE_TOO_LARGE")
		ackLogErr(d)
		return nil
	}
	// No defer here — we close explicitly below before passing the path to clamd.
	// Using both a defer and an explicit close would cause a double-close, which
	// risks closing an unrelated file descriptor that the OS recycled after the
	// first close.

	// Publish file.scan.started notification only on the first attempt (best effort — non-fatal on failure).
	if retryCount == 0 {
		startedMsg := model.ScanStartedMessage{
			MessageId:    model.NewMessageId(),
			FileId:       msg.FileId,
			CaseId:       msg.CaseId,
			OriginalName: msg.OriginalName,
			SizeBytes:    msg.SizeBytes,
			StartedAt:    time.Now().UTC(),
		}
		startedEnvelope := &mqmodel.JSONMessage[model.ScanStartedMessage]{
			Payload: startedMsg,
			Headers: amqp.Table{"__TypeId__": model.TypeIdFileScanStarted},
		}
		pubCtx, pubCancel := publishCtx(ctx, 15*time.Second)
		if err := s.pub.Publish(pubCtx, s.cfg.Exchange, s.cfg.ScanStartedRoutingKey, false, startedEnvelope); err != nil {
			log.Warn().Err(err).Msg("failed to publish ScanStartedMessage (non-fatal)")
		}
		pubCancel()
	}

	// Compute SHA-256 and magic bytes in one pass.
	sha256hex, magicAnalysis, mimeErr := computeChecksumAndMagicBytes(f, msg.OriginalName, msg.ContentType)
	if mimeErr != nil {
		log.Error().Err(mimeErr).Msg("magic byte detection failed; treating as scan error")
		return s.handleScanFailure(ctx, d, msg, retryCount, "MAGIC_BYTE_ERROR", mimeErr.Error(), d.Headers)
	}

	// Close file once reading is done; clamd reads by path.
	f.Close()

	// Post-rewind cancellation check.
	if s.isCancelled(ctx, msg.CaseId) {
		log.Info().Msg("case cancelled during file read; discarding scan job (ACK)")
		ackLogErr(d)
		return nil
	}

	// Connect to clamd and scan.
	clamClient, err := s.newClamdClient()
	if err != nil {
		log.Error().Err(err).Msg("failed to connect to clamd")
		return s.handleScanFailure(ctx, d, msg, retryCount, "CLAMD_UNAVAILABLE", err.Error(), d.Headers)
	}

	finding, err := clamClient.ScanFile(msg.TempPath)
	clamClient.Close() // Close scan connection immediately after use
	scanDuration := time.Since(start)
	if err != nil {
		if err == clamd.ErrFileNotFound {
			log.Error().Err(err).Str("tempPath", msg.TempPath).Msg("clamd could not find file; discarding (ACK)")
			ackLogErr(d)
			return nil
		}
		// Check if this is a clamd scan error (transient failure, should retry).
		if errors.Is(err, clamd.ErrClamdScanError) {
			log.Error().Err(err).Msg("clamd scan error; scheduling retry")
			return s.handleScanFailure(ctx, d, msg, retryCount, "CLAMD_SCAN_ERROR", err.Error(), d.Headers)
		}
		log.Error().Err(err).Msg("clamd scan error")
		return s.handleScanFailure(ctx, d, msg, retryCount, "SCAN_ERROR", err.Error(), d.Headers)
	}

	// Get engine and signature versions on a separate connection (best effort).
	engineVer, sigVer := s.getVersionsOnNewConnection()

	// Post-scan cancellation check: discard result if cancelled during scan.
	if s.isCancelled(ctx, msg.CaseId) {
		log.Info().Msg("case cancelled during scan; discarding result, deleting temp file")
		s.removeFileWithRetry(msg.TempPath)
		ackLogErr(d)
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
		s.removeFileWithRetry(msg.TempPath)
	}

	result := model.ScanCompletedMessage{
		MessageId:         model.NewMessageId(),
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

	envelope := &mqmodel.JSONMessage[model.ScanCompletedMessage]{
		Payload: result,
		Headers: amqp.Table{"__TypeId__": model.TypeIdFileScanCompleted},
	}
	pubCtx, pubCancel := publishCtx(ctx, 15*time.Second)
	if err := s.pub.Publish(pubCtx, s.cfg.Exchange, s.cfg.ScanCompletedRoutingKey, false, envelope); err != nil {
		log.Error().Err(err).Msg("failed to publish ScanCompletedMessage; NACKing original (transient failure, will requeue)")
		pubCancel()
		nackLogErr(d, true)
		return err
	}
	pubCancel()

	log.Info().
		Str("verdict", string(verdict)).
		Str("sha256", sha256hex).
		Int64("durationMs", scanDuration.Milliseconds()).
		Msg("scan completed, result published")

	ackLogErr(d)
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
			"__TypeId__":         "FileUploadedMessage",
		}

		// Publish a new file.uploaded message to the retry queue directly
		// (not via exchange — goes directly into the TTL queue).
		retryEnvelope := &mqmodel.JSONMessage[model.FileUploadedMessage]{
			Payload: msg,
			Headers: retryHeaders,
		}

		retryPubCtx, retryPubCancel := publishCtx(ctx, 15*time.Second)
		err := s.pub.Publish(retryPubCtx, "", queueName, false, retryEnvelope)
		retryPubCancel()
		if err != nil {
			log.Error().Err(err).
				Str("fileId", msg.FileId).
				Str("queueName", queueName).
				Msg("failed to publish retry message; NACKing (transient failure, will requeue)")
			nackLogErr(d, true)
			return err
		}

		// Publish file.scan.retrying notification for the Java Backend.
		retryNotif := model.ScanRetryingMessage{
			MessageId:        model.NewMessageId(),
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
		notifEnvelope := &mqmodel.JSONMessage[model.ScanRetryingMessage]{
			Payload: retryNotif,
			Headers: amqp.Table{"__TypeId__": model.TypeIdFileScanRetrying},
		}
		// Best effort: log but don't fail if this publish doesn't confirm.
		// Uses its own timeout so a slow retry publish cannot starve this one.
		notifPubCtx, notifPubCancel := publishCtx(ctx, 15*time.Second)
		if err := s.pub.Publish(notifPubCtx, s.cfg.Exchange, s.cfg.ScanRetryingRoutingKey, false, notifEnvelope); err != nil {
			log.Warn().Err(err).Str("fileId", msg.FileId).Msg("failed to publish ScanRetryingMessage (non-fatal)")
		}
		notifPubCancel()

		log.Warn().
			Str("fileId", msg.FileId).
			Str("caseId", msg.CaseId).
			Str("error", errorCode).
			Int("nextRetry", nextRetry).
			Int("maxRetries", maxRetries).
			Str("retryQueue", queueName).
			Msgf("scan failed, scheduling retry %d/%d", nextRetry, maxRetries)

		ackLogErr(d)
		return nil
	}

	// Retries exhausted — publish to DLX.
	failedMsg := model.ScanFailedMessage{
		MessageId:       model.NewMessageId(),
		FileId:          msg.FileId,
		CaseId:          msg.CaseId,
		Error:           errorCode,
		Message:         errorMsg,
		RetryCount:      retryCount,
		OriginalMessage: msg,
		FailedAt:        time.Now().UTC(),
	}

	dlxEnvelope := &mqmodel.JSONMessage[model.ScanFailedMessage]{
		Payload: failedMsg,
		Headers: amqp.Table{"__TypeId__": model.TypeIdFileScanFailed},
	}
	dlxPubCtx, dlxPubCancel := publishCtx(ctx, 15*time.Second)
	err := s.pub.Publish(dlxPubCtx, s.cfg.DLX, s.cfg.DLQRoutingKey, false, dlxEnvelope)
	dlxPubCancel()
	if err != nil {
		log.Error().Err(err).
			Str("fileId", msg.FileId).
			Msg("failed to publish ScanFailedMessage to DLX; NACKing (transient failure, will requeue)")
		nackLogErr(d, true)
		return err
	}

	log.Error().
		Str("fileId", msg.FileId).
		Str("caseId", msg.CaseId).
		Str("error", errorCode).
		Msgf("scan permanently failed after %d retries for file %s", retryCount, msg.FileId)

	ackLogErr(d)
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
	m := clamVersionRe.FindStringSubmatch(versionStr)
	if len(m) == 3 {
		return m[1], m[2]
	}

	return versionStr, ""
}

// getVersionsOnNewConnection opens a fresh clamd connection to retrieve version info.
// Returns empty strings on any error (best effort, non-fatal).
func (s *Scanner) getVersionsOnNewConnection() (engineVer, sigVer string) {
	c, err := s.newClamdClient()
	if err != nil {
		return "", ""
	}
	defer c.Close()
	return s.getClamdVersions(c)
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
// When the claimed MIME type is absent but a file extension is present, the extension
// is resolved to a MIME type and used as the reference for comparison.
func classifyConsistency(detected, claimed, ext string) model.MagicByteConsistency {
	if detected == "" {
		return model.ConsistencyUnknown
	}
	if claimed == "" && ext == "" {
		return model.ConsistencyEmpty
	}

	// When no claimed MIME type is provided, derive one from the file extension.
	// This prevents a misleading EMPTY verdict for files like "document.pdf" where
	// the claimed MIME type is absent but the extension clearly matches the detected type.
	reference := claimed
	if reference == "" && ext != "" {
		reference = extToMime(ext)
	}

	// If the extension is unknown we still have no reference to compare against.
	if reference == "" {
		return model.ConsistencyEmpty
	}

	detectedBase := mimeBase(detected)
	referenceBase := mimeBase(reference)

	if detectedBase == referenceBase {
		return model.ConsistencyConsistent
	}

	// Check whether the detected type is a well-known alias for the reference type.
	if mimeAlias(detected, reference) || mimeAlias(reference, detected) {
		return model.ConsistencyConsistent
	}

	// Same top-level type (e.g. both "application/*") but different subtype.
	if sameTopLevel(detected, reference) {
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

// extToMime maps a lowercase file extension (with leading dot, e.g. ".pdf") to
// a canonical MIME type string. Returns an empty string for unknown extensions.
// The table covers the file types most commonly submitted through BaNyA.
var extMimeMap = map[string]string{
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".zip":  "application/zip",
	".rar":  "application/x-rar-compressed",
	".7z":   "application/x-7z-compressed",
	".tar":  "application/x-tar",
	".gz":   "application/gzip",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".bmp":  "image/bmp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".svg":  "image/svg+xml",
	".txt":  "text/plain",
	".csv":  "text/csv",
	".xml":  "application/xml",
	".json": "application/json",
	".html": "text/html",
	".htm":  "text/html",
	".dwg":  "image/vnd.dwg",
	".dxf":  "image/vnd.dxf",
}

func extToMime(ext string) string {
	return extMimeMap[strings.ToLower(ext)]
}

// extractRetryCount reads the x-retry-count integer header from AMQP headers.
// Returns 0 if the header is absent or cannot be parsed.
// For float64 values, bounds-checks against math.MaxInt to prevent overflow.
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
		if val > math.MaxInt || val < math.MinInt {
			log.Warn().Int64("value", val).Msg("retry count out of bounds")
			return 0
		}
		return int(val)
	case int32:
		return int(val)
	case int:
		return val
	case float64:
		// Bounds-check float64 before casting to int to prevent overflow/precision loss.
		if val > float64(math.MaxInt) || val < float64(math.MinInt) || math.IsNaN(val) || math.IsInf(val, 0) {
			log.Warn().Float64("value", val).Msg("retry count out of bounds or invalid")
			return 0
		}
		// Check for fractional part (non-integer float).
		if val != math.Trunc(val) {
			log.Warn().Float64("value", val).Msg("retry count is not an integer")
			return 0
		}
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

// ackLogErr logs an error if ACK fails.
// publishRejection publishes a ScanCompletedMessage with VerdictError for a
// file that was rejected before the clamd scan could run (symlink, oversize,
// etc.). This ensures the uploader transitions the file out of SCANNING into
// a terminal ERROR state and notifies the user, instead of leaking the file
// in SCANNING forever and relying on the orphan-sweep job.
//
// Best-effort: if publishing the rejection notice fails, we log and return
// the error so the caller can decide whether to ACK-discard (accepting that
// the uploader will never know) or NACK-requeue (accepting that the broker
// will keep re-delivering a file we've already decided to reject). Both the
// current callers choose ACK-discard — a rejected file is by definition not
// going to be scanned, so redelivery would loop forever.
func (s *Scanner) publishRejection(ctx context.Context, msg model.FileUploadedMessage, reason string) error {
	result := model.ScanCompletedMessage{
		MessageId:      model.NewMessageId(),
		FileId:         msg.FileId,
		CaseId:         msg.CaseId,
		Verdict:        model.VerdictError,
		ThreatName:     reason,
		ChecksumSha256: "",
		ScannedAt:      time.Now().UTC(),
		ScanDurationMs: 0,
	}
	envelope := &mqmodel.JSONMessage[model.ScanCompletedMessage]{
		Payload: result,
		Headers: amqp.Table{"__TypeId__": model.TypeIdFileScanCompleted},
	}
	pubCtx, cancel := publishCtx(ctx, 15*time.Second)
	defer cancel()
	if err := s.pub.Publish(pubCtx, s.cfg.Exchange, s.cfg.ScanCompletedRoutingKey, false, envelope); err != nil {
		log.Error().
			Err(err).
			Str("fileId", msg.FileId).
			Str("caseId", msg.CaseId).
			Str("reason", reason).
			Msg("failed to publish rejection (VerdictError) message")
		return err
	}
	log.Info().
		Str("fileId", msg.FileId).
		Str("caseId", msg.CaseId).
		Str("reason", reason).
		Msg("published rejection (VerdictError) message to uploader")
	return nil
}

func ackLogErr(d amqp.Delivery) {
	if err := d.Ack(false); err != nil {
		log.Warn().Err(err).Uint64("deliveryTag", d.DeliveryTag).Msg("ack failed")
	}
}

// nackLogErr logs an error if NACK fails.
func nackLogErr(d amqp.Delivery, requeue bool) {
	if err := d.Nack(false, requeue); err != nil {
		log.Warn().Err(err).Uint64("deliveryTag", d.DeliveryTag).Bool("requeue", requeue).Msg("nack failed")
	}
}
