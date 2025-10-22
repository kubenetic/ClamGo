package clamd

import (
	"fmt"

	"ClamGo/pkg/model"
)

func (client *ClamClient) Ping() (response []byte, err error) {
	if client.connection == nil {
		return nil, fmt.Errorf("connection is nil")
	}

	return client.sendAndReceive(model.CmdPing)
}
