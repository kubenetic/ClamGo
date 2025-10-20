package clamd

import (
    "ClamGo/pkg/model"
    "fmt"
    "net"
    "path"
    "regexp"
    "strings"

    "github.com/rs/zerolog/log"
    "github.com/spf13/viper"
)

// parseScanResponse parses clamd scan text and returns a map[path]status where status is
// either "OK" or the malware name without the trailing "FOUND". It ignores leading
// numbering prefixes like "1:".
func parseScanResponse(line string) map[string]string {
    // Pattern matches optional leading number+colon, then a path, then colon and status
    // Examples:
    // 1: /scandir/vis.dwg: OK
    // 4: /scandir/eicar.txt: Win.Test.EICAR_HDB-1 FOUND
    re := regexp.MustCompile(`^(?:\s*\d+:\s*)?([^:]+):\s*(.+?)\s*(?:FOUND)?\s*$`)

    if strings.TrimSpace(line) == "" {
        return nil
    }

    var result = make(map[string]string)
    m := re.FindStringSubmatch(line)
    if len(m) == 3 {
        file := strings.TrimSpace(m[1])
        status := strings.TrimSpace(m[2])
        result[file] = status
    }

    return result
}

func (client *ClamClient) Scan(files ...string) (map[string]string, error) {
    connection, err := net.Dial("unix", viper.GetString("clamd.unix.path"))
    if err != nil {
        log.Error().Err(err).Msg("error connecting to clamd")
    }

    defer connection.Close()

    if err := client.sendCommand(connection, model.CmdStartSession); err != nil {
        return nil, fmt.Errorf("error starting session: %w\n", err)
    }

    var result = make(map[string]string)
    for _, file := range files {
        if !path.IsAbs(file) {
            return nil, fmt.Errorf("file path (%s) must be absolute\n", file)
        }

        scanCmd := fmt.Sprintf("n%s %s\n", model.CmdScan, file)
        if err := client.write(connection, []byte(scanCmd)); err != nil {
            return nil, fmt.Errorf("error sending scan command to check file '%s': %w\n", err, file)
        }

        response, err := client.read(connection)
        if err != nil {
            return nil, fmt.Errorf("error reading response from clamd: %w\n", err)
        }

        parsed := parseScanResponse(string(response))
        for k, v := range parsed {
            result[k] = v
        }
    }

    if err := client.sendCommand(connection, model.CmdEndSession); err != nil {
        return nil, fmt.Errorf("error stopping session: %w", err)
    }

    return result, nil
}
