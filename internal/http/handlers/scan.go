package handlers

import (
    "ClamGo/pkg/model"

    "context"
    "encoding/json"
    "net/http"

    "github.com/rs/zerolog/log"
)

func (h *ClamdHandler) Scan(ctx context.Context) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var request model.ScanRequest
        if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
            log.Err(err).Msg("error decoding request")
            respondJSON(w, http.StatusBadRequest, "Error decoding request", nil, err)

            return
        }

        defer r.Body.Close()

        result, err := h.client.Scan(request.Paths...)
        if err != nil {
            log.Err(err).Msg("error scanning files")
            respondJSON(w, http.StatusInternalServerError, "Scanning files failed", nil, err)

            return
        }

        respondJSON(w, http.StatusOK, "OK", result, nil)
    }
}

func (h *ClamdHandler) ScanStream(ctx context.Context) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {

    }
}

func (h *ClamdHandler) ContinousScan(ctx context.Context) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {

    }
}

func (h *ClamdHandler) AllMatchScan(ctx context.Context) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {

    }
}

func (h *ClamdHandler) MultiScan(ctx context.Context) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {

    }
}
