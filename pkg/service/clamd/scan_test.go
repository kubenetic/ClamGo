package clamd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ─── parseScanResponse unit tests ──────────────────────────────────────────────

func TestParseScanResponse_OK(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/scandir/vis.dwg: OK", "OK"},
		{"1: /scandir/vis.dwg: OK", "OK"},
		{"  /scandir/vis.dwg: OK  ", "OK"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := parseScanResponse(tt.input)
			if err != nil {
				t.Errorf("expected no error, got %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestParseScanResponse_FOUND(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"4: /scandir/eicar.txt: Win.Test.EICAR_HDB-1 FOUND", "Win.Test.EICAR_HDB-1"},
		{"/scandir/eicar.txt: Win.Test.EICAR_HDB-1 FOUND", "Win.Test.EICAR_HDB-1"},
		{"/scandir/weird:path/file.pdf: Trojan.Generic FOUND", "Trojan.Generic"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := parseScanResponse(tt.input)
			if err != nil {
				t.Errorf("expected no error, got %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestParseScanResponse_ERROR(t *testing.T) {
	input := "/scandir/x: File path check failure: No such file or directory. ERROR"
	result, err := parseScanResponse(input)
	if !errors.Is(err, ErrClamdScanError) {
		t.Errorf("expected ErrClamdScanError, got %v", err)
	}
	if result != input {
		t.Errorf("expected full error line %q, got %q", input, result)
	}
}

// TestParseScanResponse_Empty verifies that empty/blank responses are treated as
// scan errors, NOT as clean (C-2/F-1 security fix).
func TestParseScanResponse_Empty(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"\n",
	}

	for _, input := range tests {
		t.Run("empty:"+input, func(t *testing.T) {
			result, err := parseScanResponse(input)
			if !errors.Is(err, ErrClamdScanError) {
				t.Errorf("expected ErrClamdScanError for empty input, got err=%v result=%q", err, result)
			}
		})
	}
}

// TestParseScanResponse_RELOAD verifies that a "RELOAD" response is treated as
// a scan error, NOT as clean (C-2/F-1 security fix).
func TestParseScanResponse_RELOAD(t *testing.T) {
	result, err := parseScanResponse("RELOAD")
	if !errors.Is(err, ErrClamdScanError) {
		t.Errorf("expected ErrClamdScanError for RELOAD, got err=%v result=%q", err, result)
	}
}

// TestParseScanResponse_FOUNDWithoutColon verifies that a FOUND line without
// the ": " separator is treated as a scan error, NOT as clean (C-2/F-1 security fix).
func TestParseScanResponse_FOUNDWithoutColon(t *testing.T) {
	// "/x FOUND" has no ": " separator — must be an error, not clean.
	result, err := parseScanResponse("/x FOUND")
	if !errors.Is(err, ErrClamdScanError) {
		t.Errorf("expected ErrClamdScanError for FOUND without ': ', got err=%v result=%q", err, result)
	}
}

// TestParseScanResponse_UnknownResponse verifies that any unrecognised response
// is treated as a scan error, NOT as clean (C-2/F-1 security fix).
func TestParseScanResponse_UnknownResponse(t *testing.T) {
	unknowns := []string{
		"some random text",
		"PONG",
		"ClamAV 1.4.1/27450/Wed Feb 26 08:15:00 2026",
	}
	for _, input := range unknowns {
		t.Run(input, func(t *testing.T) {
			result, err := parseScanResponse(input)
			if !errors.Is(err, ErrClamdScanError) {
				t.Errorf("expected ErrClamdScanError for unknown response %q, got err=%v result=%q", input, err, result)
			}
		})
	}
}

func TestParseScanResponse_ColonInPath(t *testing.T) {
	// Validates that the parser correctly handles colons in the path
	// by using the LAST ": " as the delimiter
	input := "1: /scandir/weird:path/file.pdf: OK"
	result, err := parseScanResponse(input)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if result != "OK" {
		t.Errorf("expected OK, got %q", result)
	}
}

// ─── ScanFile integration tests (net.Pipe) ─────────────────────────────────────

// TestScanFile_FileNotFound validates that ScanFile correctly returns
// ErrFileNotFound when clamd reports "File path check failure: No such file
// or directory. ERROR". clamd prefixes ERROR responses with the scanned path,
// so the check must be suffix-based, not equality-based.
func TestScanFile_FileNotFound(t *testing.T) {
	// net.Pipe gives us a fully in-memory, synchronous connection pair.
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Simulate clamd: read the command, then respond with the full ERROR line
	// exactly as clamd would emit it (path prefix + error message + " ERROR\n").
	serverDone := make(chan error, 1)
	go func() {
		defer close(serverDone)
		// Consume the incoming "nSCAN /mnt/temp-nfs/missing.pdf\n" command.
		reader := bufio.NewReader(serverConn)
		if _, err := reader.ReadBytes('\n'); err != nil && !errors.Is(err, io.EOF) {
			serverDone <- err
			return
		}
		// Emit the clamd file-not-found response.
		resp := "/mnt/temp-nfs/missing.pdf: File path check failure: No such file or directory. ERROR\n"
		if _, err := serverConn.Write([]byte(resp)); err != nil {
			serverDone <- err
			return
		}
	}()

	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}
	// Tight deadline so the test cannot hang in case the stub misbehaves.
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	finding, err := client.ScanFile("/mnt/temp-nfs/missing.pdf")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got err=%v finding=%q", err, finding)
	}
	if finding != "" {
		t.Errorf("expected empty finding on ErrFileNotFound, got %q", finding)
	}

	if serr, ok := <-serverDone; ok && serr != nil {
		t.Fatalf("server stub error: %v", serr)
	}
}

// TestScanFile_Clean validates that a standard OK response yields finding="OK".
func TestScanFile_Clean(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		reader := bufio.NewReader(serverConn)
		_, _ = reader.ReadBytes('\n')
		_, _ = serverConn.Write([]byte("/mnt/temp-nfs/clean.pdf: OK\n"))
	}()

	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	finding, err := client.ScanFile("/mnt/temp-nfs/clean.pdf")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if finding != "OK" {
		t.Errorf("expected OK, got %q", finding)
	}
}

// TestScanFile_Found validates that a FOUND response yields the malware name.
func TestScanFile_Found(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		reader := bufio.NewReader(serverConn)
		_, _ = reader.ReadBytes('\n')
		_, _ = serverConn.Write([]byte("/mnt/temp-nfs/evil.bin: Win.Test.EICAR_HDB-1 FOUND\n"))
	}()

	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	finding, err := client.ScanFile("/mnt/temp-nfs/evil.bin")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if finding != "Win.Test.EICAR_HDB-1" {
		t.Errorf("expected malware name, got %q", finding)
	}
}

// TestScanFile_ScanError validates that ScanFile correctly returns ErrClamdScanError
// for non-file-not-found ERROR responses (e.g., permission denied, resource exhaustion).
func TestScanFile_ScanError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		reader := bufio.NewReader(serverConn)
		_, _ = reader.ReadBytes('\n')
		// Emit a generic ERROR response (not file-not-found).
		_, _ = serverConn.Write([]byte("/mnt/temp-nfs/file.pdf: Permission denied. ERROR\n"))
	}()

	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	finding, err := client.ScanFile("/mnt/temp-nfs/file.pdf")
	if !errors.Is(err, ErrClamdScanError) {
		t.Fatalf("expected ErrClamdScanError, got err=%v finding=%q", err, finding)
	}
	if finding != "" {
		t.Errorf("expected empty finding on ErrClamdScanError, got %q", finding)
	}
}

// ─── ScanStream tests (net.Pipe) ───────────────────────────────────────────────

// stubInstreamServer reads the INSTREAM command + all chunks from the client,
// then writes the given response line.
func stubInstreamServer(t *testing.T, serverConn net.Conn, response string) {
	t.Helper()
	go func() {
		defer serverConn.Close()
		reader := bufio.NewReader(serverConn)
		// Read the "nINSTREAM\n" command line.
		if _, err := reader.ReadBytes('\n'); err != nil {
			return
		}
		// Drain all chunks until the zero-length terminator.
		for {
			var lenBuf [4]byte
			if _, err := io.ReadFull(reader, lenBuf[:]); err != nil {
				return
			}
			chunkLen := binary.BigEndian.Uint32(lenBuf[:])
			if chunkLen == 0 {
				break
			}
			chunk := make([]byte, chunkLen)
			if _, err := io.ReadFull(reader, chunk); err != nil {
				return
			}
		}
		_, _ = serverConn.Write([]byte(response + "\n"))
	}()
}

func TestScanStream_Clean(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	stubInstreamServer(t, serverConn, "stream: OK")

	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	content := bytes.NewReader([]byte("clean file content"))
	finding, err := client.ScanStream(content)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if finding != "OK" {
		t.Errorf("expected OK, got %q", finding)
	}
}

func TestScanStream_Infected(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	stubInstreamServer(t, serverConn, "stream: Win.Test.EICAR_HDB-1 FOUND")

	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	content := bytes.NewReader([]byte("X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*"))
	finding, err := client.ScanStream(content)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if finding != "Win.Test.EICAR_HDB-1" {
		t.Errorf("expected malware name, got %q", finding)
	}
}

func TestScanStream_Error(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	stubInstreamServer(t, serverConn, "stream: Size limit reached. ERROR")

	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	content := bytes.NewReader([]byte("some content"))
	_, err := client.ScanStream(content)
	if !errors.Is(err, ErrClamdScanError) {
		t.Fatalf("expected ErrClamdScanError, got %v", err)
	}
}

// Keep a reference to strings so the import is used if the file evolves.
var _ = strings.HasSuffix
