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

func (f *ResponseFileMeta) GetFileId() string {
	return f.FileId
}

func (f *ResponseFileMeta) GetFileName() string {
	return f.Name
}

func (f *ResponseFileMeta) GetSha256() string {
	return f.Sha256
}

type ScanResponse struct {
	JobId     string             `json:"job_id"`
	Timestamp time.Time          `json:"timestamp"`
	Elapsed   int64              `json:"elapsed"`
	Files     []ResponseFileMeta `json:"files"`
}
