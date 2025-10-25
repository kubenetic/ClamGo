package model

import (
    "time"
)

type RequestFileMeta struct {
    FileId string `json:"file_id"`
    Name   string `json:"name"`
    Sha256 string `json:"sha256"`
    Size   int64  `json:"size"`
    Path   string `json:"path"`
}

type ScanRequest struct {
    JobId     string            `json:"job_id"`
    Attempts  int               `json:"attempts"`
    Timestamp time.Time         `json:"timestamp"`
    Files     []RequestFileMeta `json:"files"`
}

func (r *ScanRequest) IncrementAttempts() {
    r.Attempts++
}

func (r *ScanRequest) GetFilePaths() []string {
	paths := make([]string, len(r.Files))
	for i, f := range r.Files {
		paths[i] = f.Path
	}
	return paths
}
