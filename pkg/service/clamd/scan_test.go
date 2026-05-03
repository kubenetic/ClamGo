package clamd

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

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
			result := parseScanResponse(tt.input)
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
			result := parseScanResponse(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestParseScanResponse_ERROR(t *testing.T) {
	input := "/scandir/x: File path check failure: No such file or directory. ERROR"
	result := parseScanResponse(input)
	if result != input {
		t.Errorf("expected full error line %q, got %q", input, result)
	}
}

func TestParseScanResponse_Empty(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"\n",
	}

	for _, input := range tests {
		t.Run("empty", func(t *testing.T) {
			result := parseScanResponse(input)
			if result != "" {
				t.Errorf("expected empty string, got %q", result)
			}
		})
	}
}

func TestParseScanResponse_ColonInPath(t *testing.T) {
	// Validates that the parser correctly handles colons in the path
	// by using the LAST ": " as the delimiter
	input := "1: /scandir/weird:path/file.pdf: OK"
	result := parseScanResponse(input)
	if result != "OK" {
		t.Errorf("expected OK, got %q", result)
	}
}

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

// Keep a reference to strings so the import is used if the file evolves.
var _ = strings.HasSuffix
