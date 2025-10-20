package handlers

import (
    "net/http"

    "github.com/rs/zerolog/log"
)

func (h *ClamdHandler) Ping() http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if _, err := h.client.Ping(); err != nil {
            log.Err(err).Msg("error pinging clamd")
            respondJSON(w, http.StatusInternalServerError, "PING failed", nil, err)

            return
        }

        respondJSON(w, http.StatusOK, "OK", nil, nil)
    }
}

func (h *ClamdHandler) Version() http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        response, err := h.client.Version()
        if err != nil {
            log.Err(err).Msg("getting version information from clamd failed")
            respondJSON(
                w, http.StatusInternalServerError, "Getting version information from CalmD failed", nil, err)

            return
        }

        respondJSON(w, http.StatusOK, "OK", string(response), nil)
    }
}

func (h *ClamdHandler) Stats() http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        metrics, err := h.client.Stats()
        if err != nil {
            log.Err(err).Msg("error getting stats from clamd")
            respondJSON(w, http.StatusInternalServerError, "Getting stats from CalmD failed", nil, err)

            return
        }

        respondJSON(w, http.StatusOK, "OK", metrics, nil)
    }
}
