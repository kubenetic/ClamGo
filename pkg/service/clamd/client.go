// Package clamd provides a thin client for communicating with a running clamd (ClamAV daemon)
// over an existing net.Conn. It implements helpers to send commands using clamd's "n<COMMAND>\n"
// protocol framing and to read responses.
package clamd

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"

	"ClamGo/pkg/model"

	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
)

// clamdDialTimeout is the maximum time allowed to establish a connection to clamd.
const clamdDialTimeout = 10 * time.Second

// clamdIOTimeout is the deadline applied to the clamd connection at connect time.
// This allows sufficient time for large file scans without timing out mid-operation.
const clamdIOTimeout = 10 * time.Minute

// clamdMaxMultiLineBytes is the maximum size of a multi-line response from clamd.
// Protects against unbounded responses.
const clamdMaxMultiLineBytes = 64 * 1024

// ClamClient wraps a network mqConn to clamd and provides convenience methods
// to send commands and read responses. The client does not manage a mqConn
// establishment; callers must supply a ready net.Conn.
type ClamClient struct {
	connection net.Conn
	reader     *bufio.Reader
}

// NewClamClient returns a new ClamClient connected to clamd using the address
// configured in Viper. It checks the leaf keys "clamd.unix.path" and
// "clamd.tcp.addr" (not the section keys "clamd.unix" / "clamd.tcp") so that
// viper.IsSet returns true whenever the value is actually present.
func NewClamClient() (*ClamClient, error) {
	client := &ClamClient{}
	if viper.IsSet("clamd.unix.path") && viper.GetString("clamd.unix.path") != "" {
		err := client.Connect("unix", viper.GetString("clamd.unix.path"))
		return client, err
	} else if viper.IsSet("clamd.tcp.addr") && viper.GetString("clamd.tcp.addr") != "" {
		err := client.Connect("tcp", viper.GetString("clamd.tcp.addr"))
		return client, err
	} else {
		return nil, fmt.Errorf("no connection configuration found")
	}
}

func (client *ClamClient) Connect(proto string, addr string) error {
	conn, err := net.DialTimeout(proto, addr, clamdDialTimeout)
	if err != nil {
		return err
	}
	// Set a deadline on every subsequent I/O operation so that a hung clamd
	// (e.g. scanning a very large file or a network partition after the TCP
	// handshake) cannot block the scan goroutine indefinitely.
	if err := conn.SetDeadline(time.Now().Add(clamdIOTimeout)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("set clamd connection deadline: %w", err)
	}
	client.connection = conn
	client.reader = bufio.NewReader(conn)
	return nil
}

// Close closes the underlying network connection to clamd.
// It is safe to call Close on a client whose Connect call failed or was never
// called — in that case it is a no-op.
func (client *ClamClient) Close() error {
	if client.connection == nil {
		return nil
	}
	return client.connection.Close()
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

// read reads a single newline-terminated response line from clamd.
// Used for single-line commands (PING, VERSION, SCAN).
func (client *ClamClient) read() (response []byte, err error) {
	response, err = client.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}

	log.Debug().
		Int("received", len(response)).
		Msg("response received")

	return
}

// readMultiLine reads clamd responses that span multiple newline-terminated lines
// and are terminated by a line containing only "END". Used for the STATS command.
func (client *ClamClient) readMultiLine() ([]byte, error) {
	var buf []byte
	for {
		line, err := client.reader.ReadBytes('\n')
		buf = append(buf, line...)
		if len(buf) > clamdMaxMultiLineBytes {
			return buf, fmt.Errorf("clamd multi-line response exceeded %d bytes", clamdMaxMultiLineBytes)
		}
		if err != nil {
			// EOF or deadline — return whatever we have accumulated.
			return buf, err
		}
		// clamd terminates STATS output with a bare "END\n" line.
		if strings.TrimSpace(string(line)) == "END" {
			break
		}
	}

	log.Debug().
		Int("received", len(buf)).
		Msg("multi-line response received")

	return buf, nil
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
