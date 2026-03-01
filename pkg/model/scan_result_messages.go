package model

import "time"

// Verdict represents the outcome of a ClamAV scan.
type Verdict string

const (
	VerdictClean    Verdict = "CLEAN"
	VerdictInfected Verdict = "INFECTED"
	VerdictError    Verdict = "ERROR"
)

// MagicByteConsistency describes how well the detected MIME type matches the claimed one.
type MagicByteConsistency string

const (
	ConsistencyConsistent    MagicByteConsistency = "CONSISTENT"
	ConsistencyMinorMismatch MagicByteConsistency = "MINOR_MISMATCH"
	ConsistencyMismatch      MagicByteConsistency = "MISMATCH"
	ConsistencyUnknown       MagicByteConsistency = "UNKNOWN"
	ConsistencyEmpty         MagicByteConsistency = "EMPTY"
)

// MagicByteAnalysis holds the result of magic byte / MIME type inspection.
type MagicByteAnalysis struct {
	DetectedMimeType string               `json:"detectedMimeType"`
	ClaimedMimeType  string               `json:"claimedMimeType"`
	ClaimedExtension string               `json:"claimedExtension"`
	Consistency      MagicByteConsistency `json:"consistency"`
	Note             string               `json:"note,omitempty"`
}

// ScanCompletedMessage is published to uploader.exchange with routing key
// file.scan.completed after ClamGo finishes scanning a file (success or infected).
type ScanCompletedMessage struct {
	FileId            string            `json:"fileId"`
	CaseId            string            `json:"caseId"`
	Verdict           Verdict           `json:"verdict"`
	ThreatName        string            `json:"threatName,omitempty"`
	ChecksumSha256    string            `json:"checksumSha256"`
	MagicByteAnalysis MagicByteAnalysis `json:"magicByteAnalysis"`
	EngineVersion     string            `json:"engineVersion"`
	SignatureVersion  string            `json:"signatureVersion"`
	ScannedAt         time.Time         `json:"scannedAt"`
	ScanDurationMs    int64             `json:"scanDurationMs"`
}

// ScanRetryingMessage is published to uploader.exchange with routing key
// file.scan.retrying each time a scan fails and is scheduled for retry.
// Consumed from q.scan.results by the Java Backend.
type ScanRetryingMessage struct {
	FileId           string    `json:"fileId"`
	CaseId           string    `json:"caseId"`
	RetryAttempt     int       `json:"retryAttempt"`
	MaxRetries       int       `json:"maxRetries"`
	Error            string    `json:"error"`
	Message          string    `json:"message"`
	NextRetryQueue   string    `json:"nextRetryQueue,omitempty"`
	NextRetryDelayMs int64     `json:"nextRetryDelayMs,omitempty"`
	FailedAt         time.Time `json:"failedAt"`
}

// RetryHistoryEntry records a single failed attempt in ScanFailedMessage.
type RetryHistoryEntry struct {
	Attempt    int       `json:"attempt"`
	Error      string    `json:"error"`
	FailedAt   time.Time `json:"failedAt"`
	RetryQueue string    `json:"retryQueue"`
}

// ScanFailedMessage is published to uploader.dlx with routing key
// file.scan.failed after all 3 retries are exhausted.
type ScanFailedMessage struct {
	FileId          string              `json:"fileId"`
	CaseId          string              `json:"caseId"`
	Error           string              `json:"error"`
	Message         string              `json:"message"`
	RetryCount      int                 `json:"retryCount"`
	RetryHistory    []RetryHistoryEntry `json:"retryHistory"`
	OriginalMessage FileUploadedMessage `json:"originalMessage"`
	FailedAt        time.Time           `json:"failedAt"`
}
