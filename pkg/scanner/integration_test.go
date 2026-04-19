//go:build integration

// Package scanner integration tests exercise the Scanner against real RabbitMQ
// and Redis containers managed by testcontainers. They validate:
//   - RabbitMQ message consumption and publication (cancel messages, scan messages)
//   - Redis-backed cancellation detection (key exists, key missing, caching)
//   - Graceful degradation when Redis is unavailable (in-memory fallback)
//   - Combined flows: cancel via Redis while processing scan messages
package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"ClamGo/pkg/model"

	mqmodel "github.com/kubenetic/BunnyShepherd/pkg/model"
	rmq "github.com/kubenetic/BunnyShepherd/pkg/rabbitmq"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ─── Shared test infrastructure ────────────────────────────────────────────────

// testInfra holds container references and connection details shared across tests.
var testInfra struct {
	rabbitMQContainer testcontainers.Container
	redisContainer    testcontainers.Container
	amqpURI           string
	redisAddr         string
}

const (
	testExchange     = "uploader.exchange"
	testDLX          = "uploader.dlx"
	testScanQueue    = "q.file.scan"
	testCancelQueue  = "q.case.cancelled"
	testResultsQueue = "q.scan.results" // collects all outbound messages for assertions

	testScanCompletedRK = "file.scan.completed"
	testScanRetryingRK  = "file.scan.retrying"
	testScanFailedRK    = "file.scan.failed"
	testScanStartedRK   = "file.scan.started"
)

// TestMain starts RabbitMQ and Redis containers, declares the AMQP topology,
// runs all integration tests, and tears down containers.
func TestMain(m *testing.M) {
	ctx := context.Background()

	// ── Start RabbitMQ container ──
	rabbitC, err := testcontainers.Run(ctx, "docker.io/rabbitmq:4.1-management-alpine",
		testcontainers.WithExposedPorts("5672/tcp", "15672/tcp"),
		testcontainers.WithEnv(map[string]string{
			"RABBITMQ_DEFAULT_USER": "guest",
			"RABBITMQ_DEFAULT_PASS": "guest",
		}),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5672/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start RabbitMQ container: %v\n", err)
		os.Exit(1)
	}
	testInfra.rabbitMQContainer = rabbitC

	amqpHost, _ := rabbitC.Host(ctx)
	amqpPort, _ := rabbitC.MappedPort(ctx, "5672/tcp")
	testInfra.amqpURI = fmt.Sprintf("amqp://guest:guest@%s:%s/", amqpHost, amqpPort.Port())

	// ── Start Redis container ──
	redisC, err := testcontainers.Run(ctx, "docker.io/redis:7-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("6379/tcp").WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start Redis container: %v\n", err)
		_ = rabbitC.Terminate(ctx)
		os.Exit(1)
	}
	testInfra.redisContainer = redisC

	redisHost, _ := redisC.Host(ctx)
	redisPort, _ := redisC.MappedPort(ctx, "6379/tcp")
	testInfra.redisAddr = fmt.Sprintf("%s:%s", redisHost, redisPort.Port())

	// ── Declare AMQP topology ──
	if err := declareTopology(testInfra.amqpURI); err != nil {
		fmt.Fprintf(os.Stderr, "failed to declare AMQP topology: %v\n", err)
		_ = rabbitC.Terminate(ctx)
		_ = redisC.Terminate(ctx)
		os.Exit(1)
	}

	// ── Run tests ──
	code := m.Run()

	// ── Teardown ──
	_ = rabbitC.Terminate(ctx)
	_ = redisC.Terminate(ctx)

	os.Exit(code)
}

// declareTopology creates exchanges, queues, and bindings matching the
// production RabbitMQ topology used by ClamGo.
func declareTopology(amqpURI string) error {
	conn, err := amqp.Dial(amqpURI)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	defer ch.Close()

	// Exchanges
	if err := ch.ExchangeDeclare(testExchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", testExchange, err)
	}
	if err := ch.ExchangeDeclare(testDLX, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", testDLX, err)
	}

	// Inbound queues
	if _, err := ch.QueueDeclare(testScanQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare queue %s: %w", testScanQueue, err)
	}
	if _, err := ch.QueueDeclare(testCancelQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare queue %s: %w", testCancelQueue, err)
	}

	// Results queue — binds to all outbound routing keys so tests can consume results
	if _, err := ch.QueueDeclare(testResultsQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare queue %s: %w", testResultsQueue, err)
	}
	for _, rk := range []string{testScanCompletedRK, testScanRetryingRK, testScanStartedRK} {
		if err := ch.QueueBind(testResultsQueue, rk, testExchange, false, nil); err != nil {
			return fmt.Errorf("bind %s to %s/%s: %w", testResultsQueue, testExchange, rk, err)
		}
	}

	// DLQ results queue — binds to DLX for failed messages
	dlqQueue := "q.scan.dlq.results"
	if _, err := ch.QueueDeclare(dlqQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare queue %s: %w", dlqQueue, err)
	}
	if err := ch.QueueBind(dlqQueue, testScanFailedRK, testDLX, false, nil); err != nil {
		return fmt.Errorf("bind %s to %s/%s: %w", dlqQueue, testDLX, testScanFailedRK, err)
	}

	// Retry queues (direct publish, no exchange binding needed — scanner publishes directly)
	for i := 1; i <= 3; i++ {
		qName := fmt.Sprintf("q.file.scan.retry-%d", i)
		if _, err := ch.QueueDeclare(qName, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare queue %s: %w", qName, err)
		}
	}

	return nil
}

// newRedisClient creates a single-node Redis client connected to the test container.
func newRedisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: testInfra.redisAddr,
	})
}

// newScannerConfig returns a Config suitable for integration tests.
// TempNFSPrefix is set to the OS temp dir so test files pass path validation.
func newScannerConfig() Config {
	return Config{
		TempNFSPrefix:           os.TempDir(),
		Exchange:                testExchange,
		DLX:                     testDLX,
		ScanCompletedRoutingKey: testScanCompletedRK,
		ScanRetryingRoutingKey:  testScanRetryingRK,
		DLQRoutingKey:           testScanFailedRK,
		ScanStartedRoutingKey:   testScanStartedRK,
		ClamdTCPAddr:            "localhost:19999", // intentionally unreachable — triggers retry path
	}
}

// purgeQueues removes all messages from the test queues to isolate tests.
func purgeQueues(t *testing.T) {
	t.Helper()
	conn, err := amqp.Dial(testInfra.amqpURI)
	require.NoError(t, err)
	defer conn.Close()

	ch, err := conn.Channel()
	require.NoError(t, err)
	defer ch.Close()

	for _, q := range []string{
		testScanQueue, testCancelQueue, testResultsQueue, "q.scan.dlq.results",
		"q.file.scan.retry-1", "q.file.scan.retry-2", "q.file.scan.retry-3",
	} {
		_, _ = ch.QueuePurge(q, false)
	}
}

// publishRawMessage publishes a JSON-encoded message directly to a queue or exchange
// using a plain AMQP connection (bypasses BunnyShepherd for test setup).
func publishRawMessage(t *testing.T, exchange, routingKey string, msg any, headers amqp.Table) {
	t.Helper()
	conn, err := amqp.Dial(testInfra.amqpURI)
	require.NoError(t, err)
	defer conn.Close()

	ch, err := conn.Channel()
	require.NoError(t, err)
	defer ch.Close()

	body, err := json.Marshal(msg)
	require.NoError(t, err)

	err = ch.PublishWithContext(context.Background(), exchange, routingKey, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		Headers:     headers,
	})
	require.NoError(t, err)
}

// consumeOne reads a single message from the given queue with a timeout.
// Uses basic.get (polling) to avoid the auto-ack consumer delivery race
// that can lose messages when the connection is closed.
func consumeOne(t *testing.T, queue string, timeout time.Duration) (amqp.Delivery, bool) {
	t.Helper()
	conn, err := amqp.Dial(testInfra.amqpURI)
	require.NoError(t, err)
	defer conn.Close()

	ch, err := conn.Channel()
	require.NoError(t, err)
	defer ch.Close()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg, ok, err := ch.Get(queue, true)
		require.NoError(t, err)
		if ok {
			return msg, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return amqp.Delivery{}, false
}

// queueMessageCount returns the number of messages currently in a queue.
func queueMessageCount(t *testing.T, queue string) int {
	t.Helper()
	conn, err := amqp.Dial(testInfra.amqpURI)
	require.NoError(t, err)
	defer conn.Close()

	ch, err := conn.Channel()
	require.NoError(t, err)
	defer ch.Close()

	q, err := ch.QueueInspect(queue)
	require.NoError(t, err)
	return q.Messages
}

// ─── RabbitMQ Integration Tests ────────────────────────────────────────────────

func TestRabbitMQ_HandleCancelMessage(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithCancel(context.Background())

	// Set up BunnyShepherd connection + publisher + consumer
	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)

	s := New(newScannerConfig(), pub, nil)

	// Publish a cancel message to the cancel queue
	cancelMsg := model.CaseCancelledMessage{
		CaseId:      "cancel-test-001",
		CancelledBy: "test-user",
		CancelledAt: time.Now().UTC(),
		FileIds:     []string{"file-a", "file-b"},
	}
	publishRawMessage(t, "", testCancelQueue, cancelMsg, amqp.Table{"__TypeId__": "CaseCancelledMessage"})

	// Start cancel consumer in background
	consumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(10))
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		_ = consumer.Subscribe(ctx, testCancelQueue, rmq.GenConsumerTag("test-cancel"), s.HandleCancelMessage)
		close(done)
	}()

	// Wait for the message to be processed
	require.Eventually(t, func() bool {
		return s.isCancelledInMemory("cancel-test-001")
	}, 10*time.Second, 100*time.Millisecond, "case should be marked as cancelled")

	// Shutdown: cancel context first, then close resources
	cancel()
	<-done
	consumer.Close()
	pub.Close()
	mqConn.Close()
}

func TestRabbitMQ_HandleScanMessage_CancelledCase(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)
	defer mqConn.Close()

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)
	defer pub.Close()

	s := New(newScannerConfig(), pub, nil)

	// Pre-cancel the case
	s.MarkCancelled("cancelled-case-rmq")

	// Publish a scan message for the cancelled case
	scanMsg := model.FileUploadedMessage{
		FileId:       "file-scan-001",
		CaseId:       "cancelled-case-rmq",
		TempPath:     os.TempDir() + "/nonexistent-file.pdf",
		OriginalName: "test.pdf",
		SizeBytes:    1024,
		ContentType:  "application/pdf",
		UploadedAt:   time.Now().UTC(),
	}
	publishRawMessage(t, "", testScanQueue, scanMsg, amqp.Table{"__TypeId__": "FileUploadedMessage"})

	// Start scan consumer
	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)
	defer scanConsumer.Close()

	messageProcessed := make(chan struct{})
	handler := func(hCtx context.Context, d amqp.Delivery) error {
		err := s.HandleScanMessage(hCtx, d)
		close(messageProcessed)
		return err
	}

	go func() {
		_ = scanConsumer.Subscribe(consumerCtx, testScanQueue, rmq.GenConsumerTag("test-scan"), handler)
	}()

	// Wait for message to be processed
	select {
	case <-messageProcessed:
		// Message was ACKed (cancelled case discarded without scan)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for scan message to be processed")
	}

	// No result should be published (no scan started, no completed, no retry)
	require.Eventually(t, func() bool {
		return queueMessageCount(t, testResultsQueue) == 0
	}, 3*time.Second, 50*time.Millisecond, "no result messages should be published for cancelled case")

	consumerCancel()
}

func TestRabbitMQ_HandleScanMessage_ClamdUnavailable_TriggersRetry(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)
	defer mqConn.Close()

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)
	defer pub.Close()

	s := New(newScannerConfig(), pub, nil)

	// Create a real temp file so the scanner gets past the file-open step
	tmpFile, err := os.CreateTemp("", "clamgo-test-*.pdf")
	require.NoError(t, err)
	_, _ = tmpFile.Write([]byte("%PDF-1.4\ntest content for scan"))
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	scanMsg := model.FileUploadedMessage{
		FileId:       "file-retry-001",
		CaseId:       "case-retry-001",
		TempPath:     tmpFile.Name(),
		OriginalName: "test.pdf",
		SizeBytes:    1024,
		ContentType:  "application/pdf",
		UploadedAt:   time.Now().UTC(),
	}
	publishRawMessage(t, "", testScanQueue, scanMsg, amqp.Table{"__TypeId__": "FileUploadedMessage"})

	// Start scan consumer
	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)
	defer scanConsumer.Close()

	messageProcessed := make(chan struct{})
	handler := func(hCtx context.Context, d amqp.Delivery) error {
		err := s.HandleScanMessage(hCtx, d)
		close(messageProcessed)
		return err
	}

	go func() {
		_ = scanConsumer.Subscribe(consumerCtx, testScanQueue, rmq.GenConsumerTag("test-scan-retry"), handler)
	}()

	select {
	case <-messageProcessed:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for scan message to be processed")
	}

	// Since clamd is unreachable, scanner should route to retry queue 1
	retryMsg, ok := consumeOne(t, "q.file.scan.retry-1", 5*time.Second)
	require.True(t, ok, "expected a message on retry queue 1")

	var retried model.FileUploadedMessage
	require.NoError(t, json.Unmarshal(retryMsg.Body, &retried))
	assert.Equal(t, "file-retry-001", retried.FileId)
	assert.Equal(t, "case-retry-001", retried.CaseId)

	// Verify retry count header
	retryCount, ok := retryMsg.Headers["x-retry-count"]
	require.True(t, ok, "retry message should have x-retry-count header")
	assert.Equal(t, int64(1), retryCount)

	// A ScanStartedMessage and ScanRetryingMessage should also be published
	// to the results queue. Drain all messages and verify by __TypeId__ header.
	var startedFound, retryingFound bool
	for i := 0; i < 5; i++ {
		msg, ok := consumeOne(t, testResultsQueue, 3*time.Second)
		if !ok {
			break
		}
		typeId, _ := msg.Headers["__TypeId__"].(string)
		switch typeId {
		case "FileScanStartedMessage":
			startedFound = true
		case "FileScanRetryingMessage":
			var retryingMsg model.ScanRetryingMessage
			require.NoError(t, json.Unmarshal(msg.Body, &retryingMsg))
			assert.Equal(t, "file-retry-001", retryingMsg.FileId)
			assert.Equal(t, 1, retryingMsg.RetryAttempt)
			assert.Equal(t, 3, retryingMsg.MaxRetries)
			retryingFound = true
		}
		if startedFound && retryingFound {
			break
		}
	}
	assert.True(t, startedFound, "ScanStartedMessage should be published on first attempt")
	assert.True(t, retryingFound, "ScanRetryingMessage should be published to results queue")

	consumerCancel()
}

func TestRabbitMQ_HandleScanMessage_RetriesExhausted_PublishesToDLQ(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)
	defer mqConn.Close()

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)
	defer pub.Close()

	s := New(newScannerConfig(), pub, nil)

	// Create a real temp file
	tmpFile, err := os.CreateTemp("", "clamgo-test-dlq-*.pdf")
	require.NoError(t, err)
	_, _ = tmpFile.Write([]byte("%PDF-1.4\ntest content for DLQ"))
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	// Publish with retry count = 3 (maxRetries) — next failure should go to DLQ
	scanMsg := model.FileUploadedMessage{
		FileId:       "file-dlq-001",
		CaseId:       "case-dlq-001",
		TempPath:     tmpFile.Name(),
		OriginalName: "test.pdf",
		SizeBytes:    1024,
		ContentType:  "application/pdf",
		UploadedAt:   time.Now().UTC(),
	}
	publishRawMessage(t, "", testScanQueue, scanMsg, amqp.Table{
		"__TypeId__":    "FileUploadedMessage",
		"x-retry-count": int64(3),
	})

	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)
	defer scanConsumer.Close()

	messageProcessed := make(chan struct{})
	handler := func(hCtx context.Context, d amqp.Delivery) error {
		err := s.HandleScanMessage(hCtx, d)
		close(messageProcessed)
		return err
	}

	go func() {
		_ = scanConsumer.Subscribe(consumerCtx, testScanQueue, rmq.GenConsumerTag("test-scan-dlq"), handler)
	}()

	select {
	case <-messageProcessed:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for scan message to be processed")
	}

	// Message should appear on the DLQ
	dlqMsg, ok := consumeOne(t, "q.scan.dlq.results", 5*time.Second)
	require.True(t, ok, "expected a message on the DLQ")

	var failedMsg model.ScanFailedMessage
	require.NoError(t, json.Unmarshal(dlqMsg.Body, &failedMsg))
	assert.Equal(t, "file-dlq-001", failedMsg.FileId)
	assert.Equal(t, "case-dlq-001", failedMsg.CaseId)
	assert.Equal(t, 3, failedMsg.RetryCount)
	assert.Contains(t, failedMsg.Error, "CLAMD_UNAVAILABLE")

	consumerCancel()
}

func TestRabbitMQ_ScanStartedMessage_PublishedOnFirstAttempt(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)
	defer mqConn.Close()

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)
	defer pub.Close()

	s := New(newScannerConfig(), pub, nil)

	tmpFile, err := os.CreateTemp("", "clamgo-test-started-*.pdf")
	require.NoError(t, err)
	_, _ = tmpFile.Write([]byte("%PDF-1.4\ntest content"))
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	scanMsg := model.FileUploadedMessage{
		FileId:       "file-started-001",
		CaseId:       "case-started-001",
		TempPath:     tmpFile.Name(),
		OriginalName: "report.pdf",
		SizeBytes:    2048,
		ContentType:  "application/pdf",
		UploadedAt:   time.Now().UTC(),
	}
	// First attempt (no retry header)
	publishRawMessage(t, "", testScanQueue, scanMsg, amqp.Table{"__TypeId__": "FileUploadedMessage"})

	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)
	defer scanConsumer.Close()

	messageProcessed := make(chan struct{})
	handler := func(hCtx context.Context, d amqp.Delivery) error {
		err := s.HandleScanMessage(hCtx, d)
		close(messageProcessed)
		return err
	}

	go func() {
		_ = scanConsumer.Subscribe(consumerCtx, testScanQueue, rmq.GenConsumerTag("test-scan-started"), handler)
	}()

	select {
	case <-messageProcessed:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for scan message to be processed")
	}

	// Collect all messages from results queue — expect ScanStartedMessage + ScanRetryingMessage
	var startedFound bool
	for i := 0; i < 3; i++ {
		msg, ok := consumeOne(t, testResultsQueue, 3*time.Second)
		if !ok {
			break
		}
		typeId, _ := msg.Headers["__TypeId__"].(string)
		if typeId == "FileScanStartedMessage" {
			var started model.ScanStartedMessage
			require.NoError(t, json.Unmarshal(msg.Body, &started))
			assert.Equal(t, "file-started-001", started.FileId)
			assert.Equal(t, "case-started-001", started.CaseId)
			assert.Equal(t, "report.pdf", started.OriginalName)
			assert.Equal(t, int64(2048), started.SizeBytes)
			startedFound = true
		}
	}
	assert.True(t, startedFound, "ScanStartedMessage should be published on first attempt")

	consumerCancel()
}

func TestRabbitMQ_PublishAndConsume_BunnyShepherd_Roundtrip(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithCancel(context.Background())

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)

	// Publish a message via BunnyShepherd publisher
	testMsg := model.CaseCancelledMessage{
		CaseId:      "roundtrip-case-001",
		CancelledBy: "test",
		CancelledAt: time.Now().UTC(),
	}
	envelope := &mqmodel.JSONMessage[model.CaseCancelledMessage]{
		Payload: testMsg,
		Headers: amqp.Table{"__TypeId__": "CaseCancelledMessage"},
	}
	err = pub.Publish(ctx, "", testCancelQueue, false, envelope)
	require.NoError(t, err)

	// Consume via BunnyShepherd consumer
	consumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)

	received := make(chan model.CaseCancelledMessage, 1)

	go func() {
		_ = consumer.Subscribe(ctx, testCancelQueue, rmq.GenConsumerTag("test-roundtrip"), func(hCtx context.Context, d amqp.Delivery) error {
			var msg model.CaseCancelledMessage
			if err := json.Unmarshal(d.Body, &msg); err != nil {
				return err
			}
			received <- msg
			return d.Ack(false)
		})
	}()

	select {
	case msg := <-received:
		assert.Equal(t, "roundtrip-case-001", msg.CaseId)
		assert.Equal(t, "test", msg.CancelledBy)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for roundtrip message")
	}

	// Shutdown: cancel context first, then close resources
	cancel()
	consumer.Close()
	pub.Close()
	mqConn.Close()
}

// ─── Redis Integration Tests ───────────────────────────────────────────────────

func TestRedis_IsCancelled_KeyExists(t *testing.T) {
	ctx := context.Background()
	rc := newRedisClient()
	defer rc.Close()

	// Flush to isolate
	rc.FlushAll(ctx)

	// Set a cancelled key in Redis
	caseId := "redis-cancel-001"
	key := fmt.Sprintf("cancelled:%s", caseId)
	err := rc.Set(ctx, key, "1", 24*time.Hour).Err()
	require.NoError(t, err)

	s := New(newScannerConfig(), nil, rc)

	// Scanner should detect cancellation via Redis
	assert.True(t, s.isCancelled(ctx, caseId), "isCancelled should return true when Redis key exists")
}

func TestRedis_IsCancelled_KeyMissing(t *testing.T) {
	ctx := context.Background()
	rc := newRedisClient()
	defer rc.Close()

	rc.FlushAll(ctx)

	s := New(newScannerConfig(), nil, rc)

	// No key set — should not be cancelled
	assert.False(t, s.isCancelled(ctx, "nonexistent-case"), "isCancelled should return false when Redis key is missing")
}

func TestRedis_CachesLocally_AfterRedisHit(t *testing.T) {
	ctx := context.Background()
	rc := newRedisClient()
	defer rc.Close()

	rc.FlushAll(ctx)

	caseId := "redis-cache-001"
	key := fmt.Sprintf("cancelled:%s", caseId)
	err := rc.Set(ctx, key, "1", 24*time.Hour).Err()
	require.NoError(t, err)

	s := New(newScannerConfig(), nil, rc)

	// First call — hits Redis
	assert.True(t, s.isCancelled(ctx, caseId))

	// Verify it's now cached in memory
	s.cancelMu.RLock()
	_, inMem := s.cancelled[caseId]
	s.cancelMu.RUnlock()
	assert.True(t, inMem, "case should be cached in memory after Redis hit")

	// Delete from Redis — should still be cancelled via in-memory cache
	rc.Del(ctx, key)
	assert.True(t, s.isCancelled(ctx, caseId), "should still be cancelled from in-memory cache")
}

func TestRedis_Unavailable_FallbackToInMemory(t *testing.T) {
	ctx := context.Background()

	// Create scanner with nil Redis — simulates Redis being unavailable
	s := New(newScannerConfig(), nil, nil)

	// Not cancelled initially
	assert.False(t, s.isCancelled(ctx, "no-redis-case"))

	// Mark cancelled in memory
	s.MarkCancelled("no-redis-case")

	// Should be cancelled via in-memory
	assert.True(t, s.isCancelled(ctx, "no-redis-case"))
}

func TestRedis_Unavailable_ScannerProcessesMessages(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)
	defer mqConn.Close()

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)
	defer pub.Close()

	// Scanner with NO Redis — should still work
	s := New(newScannerConfig(), pub, nil)

	tmpFile, err := os.CreateTemp("", "clamgo-test-noredis-*.pdf")
	require.NoError(t, err)
	_, _ = tmpFile.Write([]byte("%PDF-1.4\nno redis test"))
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	scanMsg := model.FileUploadedMessage{
		FileId:       "file-noredis-001",
		CaseId:       "case-noredis-001",
		TempPath:     tmpFile.Name(),
		OriginalName: "test.pdf",
		SizeBytes:    512,
		ContentType:  "application/pdf",
		UploadedAt:   time.Now().UTC(),
	}
	publishRawMessage(t, "", testScanQueue, scanMsg, amqp.Table{"__TypeId__": "FileUploadedMessage"})

	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)
	defer scanConsumer.Close()

	messageProcessed := make(chan struct{})
	handler := func(hCtx context.Context, d amqp.Delivery) error {
		err := s.HandleScanMessage(hCtx, d)
		close(messageProcessed)
		return err
	}

	go func() {
		_ = scanConsumer.Subscribe(consumerCtx, testScanQueue, rmq.GenConsumerTag("test-noredis"), handler)
	}()

	select {
	case <-messageProcessed:
		// Scanner processed the message without Redis — clamd is unreachable so it retries
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for message processing without Redis")
	}

	// Should have a retry message (clamd unavailable) — proves scanner works without Redis
	retryMsg, ok := consumeOne(t, "q.file.scan.retry-1", 5*time.Second)
	require.True(t, ok, "scanner should still route to retry queue without Redis")

	var retried model.FileUploadedMessage
	require.NoError(t, json.Unmarshal(retryMsg.Body, &retried))
	assert.Equal(t, "file-noredis-001", retried.FileId)

	consumerCancel()
}

// ─── Combined RabbitMQ + Redis Tests ───────────────────────────────────────────

func TestCombined_CancelViaRedis_SkipsScan(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rc := newRedisClient()
	defer rc.Close()
	rc.FlushAll(ctx)

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)
	defer mqConn.Close()

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)
	defer pub.Close()

	s := New(newScannerConfig(), pub, rc)

	// Set cancellation in Redis (simulating the Java Backend setting it)
	caseId := "combined-cancel-001"
	key := fmt.Sprintf("cancelled:%s", caseId)
	err = rc.Set(ctx, key, "1", 24*time.Hour).Err()
	require.NoError(t, err)

	// Publish a scan message for this case
	scanMsg := model.FileUploadedMessage{
		FileId:       "file-combined-001",
		CaseId:       caseId,
		TempPath:     os.TempDir() + "/nonexistent.pdf",
		OriginalName: "test.pdf",
		SizeBytes:    1024,
		ContentType:  "application/pdf",
		UploadedAt:   time.Now().UTC(),
	}
	publishRawMessage(t, "", testScanQueue, scanMsg, amqp.Table{"__TypeId__": "FileUploadedMessage"})

	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)
	defer scanConsumer.Close()

	messageProcessed := make(chan struct{})
	handler := func(hCtx context.Context, d amqp.Delivery) error {
		err := s.HandleScanMessage(hCtx, d)
		close(messageProcessed)
		return err
	}

	go func() {
		_ = scanConsumer.Subscribe(consumerCtx, testScanQueue, rmq.GenConsumerTag("test-combined"), handler)
	}()

	select {
	case <-messageProcessed:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for scan message processing")
	}

	// No results should be published — case was cancelled via Redis pre-check
	require.Eventually(t, func() bool {
		return queueMessageCount(t, testResultsQueue) == 0 &&
			queueMessageCount(t, "q.file.scan.retry-1") == 0
	}, 3*time.Second, 50*time.Millisecond, "no results or retries for Redis-cancelled case")

	// Verify the case is now cached in memory too
	s.cancelMu.RLock()
	_, inMem := s.cancelled[caseId]
	s.cancelMu.RUnlock()
	assert.True(t, inMem, "Redis-cancelled case should be cached in memory after detection")

	consumerCancel()
}

func TestCombined_CancelViaRabbitMQ_ThenScanSkipped(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rc := newRedisClient()
	defer rc.Close()
	rc.FlushAll(ctx)

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)
	defer mqConn.Close()

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)
	defer pub.Close()

	s := New(newScannerConfig(), pub, rc)

	// Step 1: Send cancel message via RabbitMQ and consume it
	cancelMsg := model.CaseCancelledMessage{
		CaseId:      "combined-flow-001",
		CancelledBy: "admin",
		CancelledAt: time.Now().UTC(),
	}
	publishRawMessage(t, "", testCancelQueue, cancelMsg, amqp.Table{"__TypeId__": "CaseCancelledMessage"})

	cancelConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(10))
	require.NoError(t, err)
	defer cancelConsumer.Close()

	cancelCtx, cancelConsumerCancel := context.WithCancel(ctx)

	go func() {
		_ = cancelConsumer.Subscribe(cancelCtx, testCancelQueue, rmq.GenConsumerTag("test-combined-cancel"), s.HandleCancelMessage)
	}()

	// Wait for cancel to be processed
	require.Eventually(t, func() bool {
		return s.isCancelled(ctx, "combined-flow-001")
	}, 10*time.Second, 100*time.Millisecond)

	cancelConsumerCancel()

	// Step 2: Now send a scan message for the same case — should be skipped
	scanMsg := model.FileUploadedMessage{
		FileId:       "file-combined-flow-001",
		CaseId:       "combined-flow-001",
		TempPath:     os.TempDir() + "/nonexistent.pdf",
		OriginalName: "test.pdf",
		SizeBytes:    1024,
		ContentType:  "application/pdf",
		UploadedAt:   time.Now().UTC(),
	}
	publishRawMessage(t, "", testScanQueue, scanMsg, amqp.Table{"__TypeId__": "FileUploadedMessage"})

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)
	defer scanConsumer.Close()

	scanCtx, scanConsumerCancel := context.WithCancel(ctx)
	defer scanConsumerCancel()

	messageProcessed := make(chan struct{})
	handler := func(hCtx context.Context, d amqp.Delivery) error {
		err := s.HandleScanMessage(hCtx, d)
		close(messageProcessed)
		return err
	}

	go func() {
		_ = scanConsumer.Subscribe(scanCtx, testScanQueue, rmq.GenConsumerTag("test-combined-scan"), handler)
	}()

	select {
	case <-messageProcessed:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for scan message processing")
	}

	// No results — case was cancelled via in-memory (set by cancel consumer)
	require.Eventually(t, func() bool {
		return queueMessageCount(t, testResultsQueue) == 0
	}, 3*time.Second, 50*time.Millisecond, "no results for cancelled case")

	scanConsumerCancel()
}

func TestCombined_WithRedis_MultipleScansForSameCancelledCase(t *testing.T) {
	purgeQueues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rc := newRedisClient()
	defer rc.Close()
	rc.FlushAll(ctx)

	mqConn, err := rmq.NewConnectionManager(ctx, testInfra.amqpURI, &amqp.Config{})
	require.NoError(t, err)
	defer mqConn.Close()

	pub, err := rmq.NewPublisher(mqConn)
	require.NoError(t, err)
	defer pub.Close()

	s := New(newScannerConfig(), pub, rc)

	// Cancel via Redis
	caseId := "multi-scan-cancel-001"
	key := fmt.Sprintf("cancelled:%s", caseId)
	err = rc.Set(ctx, key, "1", 24*time.Hour).Err()
	require.NoError(t, err)

	// Publish 3 scan messages for the same cancelled case
	for i := 1; i <= 3; i++ {
		scanMsg := model.FileUploadedMessage{
			FileId:       fmt.Sprintf("file-multi-%d", i),
			CaseId:       caseId,
			TempPath:     os.TempDir() + "/nonexistent.pdf",
			OriginalName: "test.pdf",
			SizeBytes:    1024,
			ContentType:  "application/pdf",
			UploadedAt:   time.Now().UTC(),
		}
		publishRawMessage(t, "", testScanQueue, scanMsg, amqp.Table{"__TypeId__": "FileUploadedMessage"})
	}

	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(1))
	require.NoError(t, err)
	defer scanConsumer.Close()

	// Use a buffered channel as a race-free counter: each processed message
	// sends one token; the test goroutine drains exactly 3.
	processed := make(chan struct{}, 3)
	handler := func(hCtx context.Context, d amqp.Delivery) error {
		err := s.HandleScanMessage(hCtx, d)
		processed <- struct{}{}
		return err
	}

	go func() {
		_ = scanConsumer.Subscribe(consumerCtx, testScanQueue, rmq.GenConsumerTag("test-multi"), handler)
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-processed:
		case <-time.After(15 * time.Second):
			t.Fatalf("timed out waiting for message %d/3", i+1)
		}
	}

	// All 3 should be discarded — no results published
	require.Eventually(t, func() bool {
		return queueMessageCount(t, testResultsQueue) == 0 &&
			queueMessageCount(t, "q.file.scan.retry-1") == 0
	}, 3*time.Second, 50*time.Millisecond, "no results or retries for any cancelled scan")

	consumerCancel()
}
