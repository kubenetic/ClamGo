// Package clamd provides a thin client for communicating with a running clamd (ClamAV daemon)
// over an existing net.Conn. It implements helpers to send commands using clamd's "n<COMMAND>\n"
// protocol framing and to read responses.
package clamd

import (
	"bufio"
	"fmt"
	"net"

	"ClamGo/pkg/model"

	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
)

// ClamClient wraps a network mqConn to clamd and provides convenience methods
// to send commands and read responses. The client does not manage a mqConn
// establishment; callers must supply a ready net.Conn.
type ClamClient struct {
	connection net.Conn
}

// NewClamClient returns a new ClamClient that uses the provided net.Conn.
// The caller is responsible for establishing and closing the mqConn.
func NewClamClient() (*ClamClient, error) {
	client := &ClamClient{}
	if viper.IsSet("clamd.unix") {
		err := client.Connect("unix", viper.GetString("clamd.unix.path"))
		return client, err
	} else if viper.IsSet("clamd.tcp") {
		err := client.Connect("tcp", viper.GetString("clamd.tcp.addr"))
		return client, err
	} else {
		return nil, fmt.Errorf("no connection configuration found")
	}
}

func (client *ClamClient) Connect(proto string, addr string) error {
	conn, err := net.Dial(proto, addr)
	if err != nil {
		return err
	}
	client.connection = conn
	return nil
}

// Close closes the underlying network mqConn to clamd.
func (client *ClamClient) Close() error {
	if err := client.connection.Close(); err != nil {
		return err
	}

	return nil
}

// write sends raw bytes to the clamd connection and logs the number of bytes written.
// It returns any error encountered while writing.
func (client *ClamClient) write(command []byte) error {
	bytesWritten, err := client.connection.Write(command)
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
func (client *ClamClient) sendCommand(command model.ClamDCommand) error {
	wrappedCommand := fmt.Sprintf("n%s\n", command)

	if err := client.write([]byte(wrappedCommand)); err != nil {
		return err
	}

	return nil
}

// read reads from the clamd connection in chunks until no more data is available
// or an EOF is encountered. It aggregates all received bytes into a single
// slice and returns it. Logging includes the total number of bytes received.
func (client *ClamClient) read() (response []byte, err error) {
	reader := bufio.NewReader(client.connection)
	response, err = reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}

	log.Debug().
		Int("received", len(response)).
		Msg("response received")

	return
}

func (client *ClamClient) sendAndReceive(command model.ClamDCommand) (response []byte, err error) {
	if err = client.sendCommand(command); err != nil {
		return
	}

	if response, err = client.read(); err != nil {
		return
	}

	return
}
