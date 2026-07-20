package clamd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"ClamGo/pkg/model"
)

// instreamChunkSize is the chunk size used when streaming file content to clamd via INSTREAM.
// clamd's default StreamMaxLength is 25 MB; we use 4 KB chunks for efficient I/O.
const instreamChunkSize = 4096

var (
	ErrFileNotFound   = errors.New("file not found")
	ErrClamdScanError = errors.New("clamd scan error")
)

// parseScanResponse parses a single clamd scan response line and returns:
//   - ("OK", nil)          — line ends with " OK" (clean)
//   - (malwareName, nil)   — line ends with " FOUND" and contains ": " separator
//   - (line, ErrClamdScanError) — line ends with " ERROR"
//   - ("", ErrClamdScanError)  — empty/blank line, "RELOAD", FOUND without ": ",
//     or any other unrecognised response
//
// SECURITY: an unrecognised or empty response MUST NOT be treated as clean.
// Only an explicit " OK" suffix yields VerdictClean.
func parseScanResponse(line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("%w: empty response from clamd", ErrClamdScanError)
	}

	if strings.HasSuffix(line, " ERROR") {
		return line, ErrClamdScanError
	}

	if strings.HasSuffix(line, " FOUND") {
		// Extract malware name: substring between the LAST ": " and " FOUND"
		lastColon := strings.LastIndex(line, ": ")
		if lastColon < 0 {
			// Malformed FOUND line — no ": " separator; treat as scan error.
			return "", fmt.Errorf("%w: malformed FOUND response (no ': ' separator): %s", ErrClamdScanError, line)
		}
		malwareName := line[lastColon+2 : len(line)-6] // -6 for " FOUND"
		return malwareName, nil
	}

	if strings.HasSuffix(line, " OK") {
		return "OK", nil
	}

	// Anything else (e.g. "RELOAD", unknown protocol message) is an error.
	return "", fmt.Errorf("%w: unrecognised clamd response: %s", ErrClamdScanError, line)
}

func (client *ClamClient) ScanFile(filePath string) (string, error) {
	if client.connection == nil {
		return "", fmt.Errorf("mqConn is nil")
	}

	if !filepath.IsAbs(filePath) {
		return "", fmt.Errorf("file path (%s) must be absolute", filePath)
	}

	scanCmd := fmt.Sprintf("n%s %s\n", model.CmdScan, filePath)
	if err := client.write([]byte(scanCmd)); err != nil {
		return "", fmt.Errorf("error sending scan command to check file '%s': %w", filePath, err)
	}

	response, err := client.read()
	if err != nil {
		return "", fmt.Errorf("error reading response from clamd: %w", err)
	}

	finding, parseErr := parseScanResponse(string(response))

	// clamd prefixes ERROR responses with the scanned path, e.g.
	//   "/mnt/temp-nfs/abc: File path check failure: No such file or directory. ERROR"
	// so compare against the suffix rather than the bare sentence.
	if strings.HasSuffix(finding, "File path check failure: No such file or directory. ERROR") {
		return "", ErrFileNotFound
	}

	if parseErr != nil {
		return "", parseErr
	}

	return finding, nil
}

// ScanStream sends file content to clamd using the INSTREAM command, which
// streams the file bytes over the existing socket connection. This eliminates
// the TOCTOU race between checksum computation and the path-based SCAN command
// because the same open file descriptor is used for both operations.
//
// Protocol: "nINSTREAM\n" followed by chunks of the form:
//
//	[4-byte big-endian length][data bytes]
//
// Terminated by a zero-length chunk: [0x00 0x00 0x00 0x00].
// clamd responds with "stream: OK\n" or "stream: <malware> FOUND\n" or an ERROR line.
func (client *ClamClient) ScanStream(r io.Reader) (string, error) {
	if client.connection == nil {
		return "", fmt.Errorf("connection is nil")
	}

	// Send INSTREAM command.
	if err := client.write([]byte("nINSTREAM\n")); err != nil {
		return "", fmt.Errorf("error sending INSTREAM command: %w", err)
	}

	// Stream file content in chunks.
	buf := make([]byte, instreamChunkSize)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			// Write 4-byte big-endian length prefix.
			var lenBuf [4]byte
			binary.BigEndian.PutUint32(lenBuf[:], uint32(n))
			if _, err := client.connection.Write(lenBuf[:]); err != nil {
				return "", fmt.Errorf("error writing chunk length: %w", err)
			}
			if _, err := client.connection.Write(buf[:n]); err != nil {
				return "", fmt.Errorf("error writing chunk data: %w", err)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", fmt.Errorf("error reading file for INSTREAM: %w", readErr)
		}
	}

	// Send zero-length terminator chunk.
	if _, err := client.connection.Write([]byte{0, 0, 0, 0}); err != nil {
		return "", fmt.Errorf("error sending INSTREAM terminator: %w", err)
	}

	// Read clamd response.
	response, err := client.read()
	if err != nil {
		return "", fmt.Errorf("error reading INSTREAM response from clamd: %w", err)
	}

	finding, parseErr := parseScanResponse(string(response))
	if parseErr != nil {
		return "", parseErr
	}

	return finding, nil
}
