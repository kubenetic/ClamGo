package model

import "time"

// CaseCancelledMessage is consumed from q.case.cancelled.
// Published by the Java Backend when a case is cancelled.
type CaseCancelledMessage struct {
	CaseId      string    `json:"caseId"`
	CancelledBy string    `json:"cancelledBy"`
	CancelledAt time.Time `json:"cancelledAt"`
	FileIds     []string  `json:"fileIds"`
}
