package model

import "time"

type HttpResponse struct {
    Timestamp time.Time   `json:"timestamp"`
    Message   string      `json:"message"`
    Status    int         `json:"status"`
    Error     string      `json:"error,omitempty"`
    Data      interface{} `json:"data,omitempty"`
}
