package clamd

import (
	"ClamGo/pkg/model"
)

func (client *ClamClient) OpenSession() error {
	if err := client.sendCommand(model.CmdStartSession); err != nil {
		return err
	}
	return nil
}

func (client *ClamClient) CloseSession() error {
	if err := client.sendCommand(model.CmdEndSession); err != nil {
		return err
	}
	return nil
}
