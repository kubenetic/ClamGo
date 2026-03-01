package clamd

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"ClamGo/pkg/model"
)

var ErrFileNotFound = fmt.Errorf("file not found")

// parseScanResponse parses clamd scan text and returns a map[path]status where status is
// either "OK" or the malware name without the trailing "FOUND". It ignores leading
// numbering prefixes like "1:".
func parseScanResponse(line string) string {
	// Pattern matches optional leading number+colon, then a path, then colon and status
	// Examples:
	// 1: /scandir/vis.dwg: OK
	// 4: /scandir/eicar.txt: Win.Test.EICAR_HDB-1 FOUND
	re := regexp.MustCompile(`^(?:\s*\d+:\s*)?([^:]+):\s*(.+?)\s*(?:FOUND)?\s*$`)

	if strings.TrimSpace(line) == "" {
		return ""
	}

	m := re.FindStringSubmatch(line)
	if len(m) == 3 {
		return strings.TrimSpace(m[2])
	}

	return ""
}

func (client *ClamClient) ScanFile(filePath string) (string, error) {
	if client.connection == nil {
		return "", fmt.Errorf("mqConn is nil")
	}

	if !path.IsAbs(filePath) {
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

	if finding == "File path check failure: No such file or directory. ERROR" {
		return "", ErrFileNotFound
	}

	return finding, nil
}
