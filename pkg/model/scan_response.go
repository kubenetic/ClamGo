package model

import (
	"time"
)

type ResponseFileMeta struct {
	FileId    string `json:"file_id"`
	Name      string `json:"name"`
	Sha256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Verdict   string `json:"verdict"`
	Signature string `json:"signature"`
	Elapsed   int64  `json:"elapsed"`
}

type ScanResponse struct {
	JobId     string             `json:"job_id"`
	Timestamp time.Time          `json:"timestamp"`
	Elapsed   int64              `json:"elapsed"`
	Files     []ResponseFileMeta `json:"files"`
}
