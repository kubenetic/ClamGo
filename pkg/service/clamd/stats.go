package clamd

import (
	"fmt"
	"regexp"
	"strconv"

	"ClamGo/pkg/model"
)

func (client *ClamClient) Stats() (*model.ClamMetrics, error) {
	if client.connection == nil {
		return nil, fmt.Errorf("connection is nil")
	}

	response, err := client.sendAndReceive(model.CmdStats)
	if err != nil {
		return nil, err
	}

	return extractClamMetrics(response), nil
}

// extractClamMetrics parses clamd STATS response into ClamMetrics using regex patterns.
// Example response lines:
// POOLS: 1
// STATE: VALID PRIMARY
// THREADS: live 1  idle 0 max 10 idle-timeout 30
// QUEUE: 0 items
//
//	STATS 0.000025
//
// MEMSTATS: heap N/A mmap N/A used N/A free N/A releasable N/A pools 1 pools_used 1371.962M pools_total 1372.007M
func extractClamMetrics(b []byte) *model.ClamMetrics {
	// Convert to string once for regex processing
	s := string(b)
	var m model.ClamMetrics

	// POOLS
	if re := regexp.MustCompile(`(?m)^POOLS:\s*(\d+)`); true {
		if g := re.FindStringSubmatch(s); len(g) == 2 {
			m.SetPools(atoi(g[1]))
		}
	}

	// STATE (VALID/INVALID/EXIT/??), ignore extra token like PRIMARY
	if re := regexp.MustCompile(`(?m)^STATE:\s*([A-Z?]+)`); true {
		if g := re.FindStringSubmatch(s); len(g) == 2 {
			m.SetState(model.ClamdState(g[1]))
		}
	}

	// THREADS
	if re := regexp.MustCompile(`(?m)^THREADS:\s*live\s*(\d+)\s*idle\s*(\d+)\s*max\s*(\d+)\s*idle-timeout\s*(\d+)`); true {
		if g := re.FindStringSubmatch(s); len(g) == 5 {
			m.SetThreads(atoi(g[1]), atoi(g[2]), atoi(g[3]), atoi(g[4]))
		}
	}

	// QUEUE
	if re := regexp.MustCompile(`(?m)^QUEUE:\s*(\d+)\s*items`); true {
		if g := re.FindStringSubmatch(s); len(g) == 2 {
			m.SetQueueLength(atoi(g[1]))
		}
	}

	// MEMSTATS numbers may be N/A or floats optionally with M suffix
	if re := regexp.MustCompile(`(?m)^MEMSTATS:\s*heap\s*([^\s]+)\s*mmap\s*([^\s]+)\s*used\s*([^\s]+)\s*free\s*([^\s]+)\s*releasable\s*([^\s]+)\s*pools\s*(\d+)\s*pools_used\s*([^\s]+)\s*pools_total\s*([^\s]+)`); true {
		if g := re.FindStringSubmatch(s); len(g) == 9 {
			heap := atofWithSuffix(g[1])
			mmap := atofWithSuffix(g[2])
			used := atofWithSuffix(g[3])
			free := atofWithSuffix(g[4])
			rel := atofWithSuffix(g[5])
			pools := atoi(g[6])
			poolsUsed := atofWithSuffix(g[7])
			poolsTotal := atofWithSuffix(g[8])
			m.SetMemStats(heap, mmap, used, free, rel, pools, poolsUsed, poolsTotal)
		}
	}

	return &m
}

func atoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// atofWithSuffix converts strings like "1371.962M" or "N/A" to float64; M suffix means megabytes; N/A -> 0.
func atofWithSuffix(s string) float64 {
	if s == "N/A" || s == "NA" || s == "-" {
		return 0
	}
	mult := 1.0
	if len(s) > 0 {
		last := s[len(s)-1]
		switch last {
		case 'K', 'k':
			mult = 1024
			s = s[:len(s)-1]
		case 'M', 'm':
			mult = 1024 * 1024
			s = s[:len(s)-1]
		case 'G', 'g':
			mult = 1024 * 1024 * 1024
			s = s[:len(s)-1]
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f * mult
}
