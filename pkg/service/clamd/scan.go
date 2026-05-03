package clamd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"ClamGo/pkg/model"
)

var (
	ErrFileNotFound   = errors.New("file not found")
	ErrClamdScanError = errors.New("clamd scan error")
)

// parseScanResponse parses clamd scan response lines and returns:
//   - "OK" if the line ends with " OK"
//   - the malware name if the line ends with " FOUND"
//   - the full line if it ends with " ERROR"
//   - "" otherwise
func parseScanResponse(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}

	if strings.HasSuffix(line, " ERROR") {
		return line
	}

	if strings.HasSuffix(line, " FOUND") {
		// Extract malware name: substring between the LAST ": " and " FOUND"
		lastColon := strings.LastIndex(line, ": ")
		if lastColon >= 0 {
			malwareName := line[lastColon+2 : len(line)-6] // -6 for " FOUND"
			return malwareName
		}
		return ""
	}

	if strings.HasSuffix(line, " OK") {
		return "OK"
	}

	return ""
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

	finding := parseScanResponse(string(response))

	// clamd prefixes ERROR responses with the scanned path, e.g.
	//   "/mnt/temp-nfs/abc: File path check failure: No such file or directory. ERROR"
	// so compare against the suffix rather than the bare sentence.
	if strings.HasSuffix(finding, "File path check failure: No such file or directory. ERROR") {
		return "", ErrFileNotFound
	}

	// Detect ERROR responses (but not the file-not-found case handled above).
	// ERROR responses end with " ERROR" and indicate transient failures.
	if strings.HasSuffix(finding, " ERROR") {
		return "", fmt.Errorf("%w: %s", ErrClamdScanError, finding)
	}

	return finding, nil
}
