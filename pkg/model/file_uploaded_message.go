// Package model defines the RabbitMQ message types used by ClamGo.
package model

import "time"

// FileUploadedMessage is the inbound message ClamGo consumes from q.file.scan.
// Published by tusd-token-hook after a file is fully uploaded.
type FileUploadedMessage struct {
	FileId       string    `json:"fileId"`
	CaseId       string    `json:"caseId"`
	TempPath     string    `json:"tempPath"`
	OriginalName string    `json:"originalName"`
	SizeBytes    int64     `json:"sizeBytes"`
	ContentType  string    `json:"contentType"`
	UploadedAt   time.Time `json:"uploadedAt"`
}
