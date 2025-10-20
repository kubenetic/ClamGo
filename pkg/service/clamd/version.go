package clamd

import (
    "ClamGo/pkg/model"
    "net"

    "github.com/rs/zerolog/log"
    "github.com/spf13/viper"
)

func (client *ClamClient) Version() (response []byte, err error) {
    connection, err := net.Dial("unix", viper.GetString("clamd.unix.path"))
    if err != nil {
        log.Error().Err(err).Msg("error connecting to clamd")
    }

    defer connection.Close()

    return client.sendAndReceive(connection, model.CmdVersion)
}
