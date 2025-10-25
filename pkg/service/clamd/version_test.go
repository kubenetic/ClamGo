package clamd

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestVersion(t *testing.T) {
	t.Parallel()
	const version = "1.5.1"

	ctx := context.Background()
	clamd, err := testcontainers.Run(ctx, "docker.io/clamav/clamav:"+version,
		testcontainers.WithExposedPorts("3310/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("3310/tcp"),
			wait.ForLog("socket found, clamd started."),
		),
	)
	defer testcontainers.CleanupContainer(t, clamd)
	if err != nil {
		t.Fatal(err)
	}

	endpoint, err := clamd.Endpoint(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	connection, err := net.Dial("tcp", endpoint)
	if err != nil {
		t.Error(err)
	}
	defer connection.Close()

	cm := ClamClient{
		connection: connection,
	}
	resp, err := cm.Version()
	if err != nil {
		t.Error(err)
	}

	if !strings.HasPrefix(string(resp), "ClamAV "+version) {
		t.Errorf("unexpected version: %s", string(resp))
	}
}
