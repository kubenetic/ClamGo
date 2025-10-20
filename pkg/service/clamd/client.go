// Package clamd provides a thin client for communicating with a running clamd (ClamAV daemon)
// over an existing net.Conn. It implements helpers to send commands using clamd's "n<COMMAND>\n"
// protocol framing and to read responses.
package clamd

import (
    "ClamGo/pkg/model"
    "bufio"
    "fmt"
    "net"

    "github.com/rs/zerolog/log"
)

// ClamClient wraps a network connection to clamd and provides convenience methods
// to send commands and read responses. The client does not manage connection
// establishment; callers must supply a ready net.Conn.
type ClamClient struct {
}

// NewClient returns a new ClamClient that uses the provided net.Conn.
// The caller is responsible for establishing and closing the connection.
func NewClient() *ClamClient {
    return &ClamClient{}
}

// Close closes the underlying network connection to clamd.
func (client *ClamClient) Close() error {
    return nil
}

// write sends raw bytes to the clamd connection and logs the number of bytes written.
// It returns any error encountered while writing.
func (client *ClamClient) write(connection net.Conn, command []byte) error {
    bytesWritten, err := connection.Write(command)
    if err != nil {
        return err
    }

    log.Debug().
        Int("bytes written", bytesWritten).
        Str("command", string(command)).
        Msg("command sent")

    return nil
}

// sendCommand formats and sends a clamd command using the required framing:
// it prefixes the command with 'n' and appends a newline ("n<COMMAND>\n").
// Returns any error encountered while writing to the connection.
func (client *ClamClient) sendCommand(connection net.Conn, command model.ClamDCommand) error {
    wrappedCommand := fmt.Sprintf("n%s\n", command)

    if err := client.write(connection, []byte(wrappedCommand)); err != nil {
        return err
    }

    return nil
}

// read reads from the clamd connection in chunks until no more data is available
// or an EOF is encountered. It aggregates all received bytes into a single
// slice and returns it. Logging includes the total number of bytes received.
func (client *ClamClient) read(connection net.Conn) (response []byte, err error) {
    reader := bufio.NewReader(connection)
    response, err = reader.ReadBytes('\n')
    if err != nil {
        return nil, err
    }

    log.Debug().
        Int("received", len(response)).
        Msg("response received")

    return
}

func (client *ClamClient) sendAndReceive(connection net.Conn, command model.ClamDCommand) (response []byte, err error) {
    if err = client.sendCommand(connection, command); err != nil {
        return
    }

    if response, err = client.read(connection); err != nil {
        return
    }

    return
}
