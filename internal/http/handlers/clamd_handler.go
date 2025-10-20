package handlers

import "ClamGo/pkg/service/clamd"

type ClamdHandler struct {
    client *clamd.ClamClient
}

func NewClamdHandler(client *clamd.ClamClient) *ClamdHandler {
    return &ClamdHandler{
        client: client,
    }
}
