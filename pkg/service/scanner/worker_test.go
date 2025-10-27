package scanner

import (
    "testing"
)

func Test_getMsgDestination(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name             string
        attempt          int
        wantedExchange   string
        wantedRoutingKey string
    }{
        {"Retry 1st time", 1, "scan.retry.x", "30s"},
        {"Retry 2nd time", 2, "scan.retry.x", "1m"},
        {"Retry 3rd time", 3, "scan.retry.x", "2m"},
        {"Retry 4th time", 4, "scan.dead.x", "dead"},
        {"Retry nth time", 5, "scan.dead.x", "dead"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            expectedExchange, expectedRoutingKey := getMsgDestination(tt.attempt)

            if expectedExchange != tt.wantedExchange {
                t.Errorf("getMsgDestination(%d) got = %v, want %v", tt.attempt, tt.wantedExchange, expectedExchange)
            }
            if expectedRoutingKey != tt.wantedRoutingKey {
                t.Errorf("getMsgDestination(%d) got1 = %v, want %v", tt.attempt, tt.wantedRoutingKey, expectedRoutingKey)
            }
        })
    }
}
