package handlers

import (
    "ClamGo/pkg/model"

    "encoding/json"
    "net/http"
    "time"

    "github.com/rs/zerolog/log"
)

func respondJSON(w http.ResponseWriter, status int, message string, data interface{}, err error) {
    response := model.HttpResponse{
        Timestamp: time.Now(),
        Message:   message,
        Status:    status,
        Data:      data,
    }

    if err != nil {
        response.Error = err.Error()
    }

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    if err := json.NewEncoder(w).Encode(response); err != nil {
        log.Err(err).Msg("error writing response")
    }
}
