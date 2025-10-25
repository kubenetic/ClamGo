package model

import (
    "time"
)

type ScanEventType string

const (
	ScanEventScanStarted            ScanEventType = "scan_started"
	ScanEventScanFinished                         = "scan_finished"
	ScanEventFileScanStarted                      = "file_scan_started"
	ScanEventFileScanFinished                     = "file_scan_finished"
	ScanEventFileScanFailed                       = "file_scan_failed"
	ScanEventFileMaxAttemptsReached               = "file_max_attempts_reached"
	ScanEventFileUploadStarted                    = "file_upload_started"
	ScanEventFileUploadFinished                   = "file_upload_finished"
	ScanEventPersisting                           = "persisting"
	ScanEventPersisted                            = "persisted"
	ScanEventUnknown                              = "??"
)

type ScanEventFileMetadata struct {
    FileId   string `json:"file_id"`
    FileName string `json:"file_name"`
    Sha256   string `json:"sha256"`
}

type ScanEvent struct {
    JobId     string                `json:"job_id"`
    Timestamp time.Time             `json:"timestamp"`
    File      ScanEventFileMetadata `json:"file"`
    Status    ScanEventType         `json:"status"`
}
