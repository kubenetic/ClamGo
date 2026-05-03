package clamd

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestReadMultiLineCap(t *testing.T) {
	// Create a pipe to simulate a clamd connection
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Create a ClamClient with the client side of the pipe
	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}

	// Start a goroutine to write a large response without END terminator
	go func() {
		// Write more than 64 KiB without an END line
		largeData := strings.Repeat("X", 65*1024)
		_, _ = serverConn.Write([]byte(largeData))
		_ = serverConn.Close()
	}()

	// Set a short deadline to avoid hanging
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	// Try to read the multi-line response
	// This should fail because the response exceeds the cap
	buf, err := client.readMultiLine()

	if err == nil {
		t.Errorf("expected error for oversized response, got nil")
	}

	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("expected error mentioning cap exceeded, got: %v", err)
	}

	// Verify we got some data before the error
	if len(buf) == 0 {
		t.Errorf("expected some buffered data before error")
	}

	if len(buf) <= clamdMaxMultiLineBytes {
		t.Errorf("expected buffer to exceed cap before error, got %d bytes", len(buf))
	}
}

func TestReadMultiLineNormal(t *testing.T) {
	// Create a pipe to simulate a clamd connection
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	// Create a ClamClient with the client side of the pipe
	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}

	// Start a goroutine to write a normal multi-line response
	go func() {
		response := "POOLS: 1\nSTATE: VALID\nEND\n"
		_, _ = serverConn.Write([]byte(response))
		_ = serverConn.Close()
	}()

	// Set a deadline
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	// Try to read the multi-line response
	buf, err := client.readMultiLine()

	if err != nil && err != io.EOF {
		t.Errorf("expected nil or EOF error, got: %v", err)
	}

	expected := "POOLS: 1\nSTATE: VALID\nEND\n"
	if string(buf) != expected {
		t.Errorf("expected %q, got %q", expected, string(buf))
	}
}

func TestReadMultiLineCapExactBoundary(t *testing.T) {
	// Test that we catch the cap exactly at the boundary
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	client := &ClamClient{
		connection: clientConn,
		reader:     bufio.NewReader(clientConn),
	}

	// Write exactly at the cap + 1 byte
	go func() {
		// Write lines that total to just over the cap
		line := strings.Repeat("X", 1000) + "\n"
		for i := 0; i < 66; i++ { // 66 * 1000 = 66000 bytes, exceeds 64*1024
			_, _ = serverConn.Write([]byte(line))
		}
		_ = serverConn.Close()
	}()

	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	buf, err := client.readMultiLine()

	if err == nil {
		t.Errorf("expected error for response exceeding cap, got nil")
	}

	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("expected error mentioning cap exceeded, got: %v", err)
	}

	if len(buf) <= clamdMaxMultiLineBytes {
		t.Errorf("expected buffer to exceed cap, got %d bytes (cap is %d)", len(buf), clamdMaxMultiLineBytes)
	}
}
