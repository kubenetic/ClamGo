package scanner

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "os/signal"
    "syscall"
    "testing"
    "time"

    "ClamGo/internal/rabbitmq"
    "ClamGo/pkg/model"

    "github.com/rabbitmq/amqp091-go"
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
    "github.com/spf13/viper"
    "github.com/testcontainers/testcontainers-go"
    "github.com/testcontainers/testcontainers-go/wait"
)

func init() {
    log.Logger = zerolog.New(os.Stdout).
        With().
        Timestamp().
        Caller().
        Logger()
}

func Test_Run(t *testing.T) {
    t.Parallel()

    const (
        clamdVersion    = "1.5.1"
        rabbitMqVersion = "4.1"
        workerCount     = 3
    )

    baseCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
    defer stop()

    // Test timeout guard to avoid hanging CI
    ctx, cancel := context.WithTimeout(baseCtx, 90*time.Second)
    defer cancel()

    clamCntr, err := startClamD(ctx, clamdVersion)
    if err != nil {
        t.Fatal(err)
    }
    defer testcontainers.CleanupContainer(t, clamCntr)
    log.Info().Msg("ClamD container started")

    clamdHost, _ := clamCntr.Host(ctx)
    clamdPort, _ := clamCntr.MappedPort(ctx, "3310/tcp")
    clamdAddr := fmt.Sprintf("%s:%s", clamdHost, clamdPort.Port())
    viper.SetDefault("clamd.tcp.addr", clamdAddr)

    mqCtnr := startMQ(err, ctx, rabbitMqVersion)
    if err != nil {
        t.Fatal(err)
    }
    defer testcontainers.CleanupContainer(t, mqCtnr)
    log.Info().Msg("RabbitMQ container started")

    rabbitHost, _ := mqCtnr.Host(ctx)
    rabbitPort, _ := mqCtnr.MappedPort(ctx, "5672/tcp")
    mqAddr := fmt.Sprintf("amqp://guest:guest@%s:%s/", rabbitHost, rabbitPort.Port())

    mqCm, err := rabbitmq.NewConnectionManager(ctx, mqAddr, nil)
    if err != nil {
        t.Fatal(err)
    }
    defer mqCm.Close()
    log.Info().Msg("RabbitMQ connection manager established")

    // Declare topology
    initMQ(t, mqAddr)

    // Start worker pool
    wp := WorkerPool{mqConn: mqCm}
    errCh := wp.Run(ctx, workerCount)
    go func() {
        for err := range errCh {
            if err == nil {
                continue
            }
            if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
                // ignore expected shutdown errors
                continue
            }
            t.Error(err)
        }
    }()
    log.Info().Msg("WorkerPool started")

    // Set up result and event collectors
    eventsCh := make(chan model.ScanEvent, 16)
    resultsCh := make(chan model.ScanResponse, 4)

    // Start consumers BEFORE publishing
    eventsConsumer, err := rabbitmq.NewConsumer(mqCm)
    if err != nil {
        t.Fatal(err)
    }
    defer eventsConsumer.Close()

    go func() {
        if err := eventsConsumer.Subscribe(ctx, "scan.events.q", "", func(c context.Context, message amqp091.Delivery) error {
            defer func() {
                _ = message.Ack(false)
            }()

            var evt model.ScanEvent
            if err := json.Unmarshal(message.Body, &evt); err != nil {
                return err
            }
            log.Info().
                Str("queue", "scan.events.q").
                Str("jobId", evt.JobId).
                Str("status", string(evt.Status)).
                Str("file", evt.File.FileName).
                Msg("event received")

            select {
            case eventsCh <- evt:
            default:
                // drop if buffer full
            }

            return nil
        }); err != nil {
            // Surface subscription errors into the test, ignore expected context cancellations
            if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
                t.Error(err)
            }
        }
    }()

    resultsConsumer, err := rabbitmq.NewConsumer(mqCm)
    if err != nil {
        t.Fatal(err)
    }
    defer resultsConsumer.Close()

    go func() {
        if err := resultsConsumer.Subscribe(ctx, "scan.results.q", "", func(c context.Context, message amqp091.Delivery) error {
            defer func() {
                _ = message.Ack(false)
            }()

            var resp model.ScanResponse
            if err := json.Unmarshal(message.Body, &resp); err != nil {
                return err
            }
            log.Info().
                Str("queue", "scan.results.q").
                Str("jobId", resp.JobId).
                Int("files", len(resp.Files)).
                Msg("result received")

            select {
            case resultsCh <- resp:
            default:
            }

            return nil
        }); err != nil {
            if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
                t.Error(err)
            }
        }
    }()

    // Prepare and publish a scan request with two files
    scanReq := model.ScanRequest{
        JobId:     "1234",
        Attempts:  1,
        Timestamp: time.Now(),
        Files: []model.RequestFileMeta{
            {FileId: "a", Name: "eicar.txt", Path: "/testfiles/eicar.txt"},
            {FileId: "b", Name: "http_request.http", Path: "/testfiles/scan_request.http"},
        },
    }

    publisher, err := rabbitmq.NewPublisher(mqCm)
    if err != nil {
        t.Fatal(err)
    }
    defer publisher.Close()
    log.Info().Msg("RabbitMQ publisher established")

    if err := publisher.Publish(ctx, "scan.jobs.x", "jobs", false, &model.JSONMessage[model.ScanRequest]{
        Payload:       scanReq,
        MessageId:     "",
        CorrelationId: "",
        Headers:       nil,
        ContentType:   "",
    }); err != nil {
        t.Fatal(err)
    }
    log.Info().Msg("ScanRequest published")

    // Await expected events and results
    type void struct{}
    done := make(chan void)

    // Expectations
    expectedEvents := map[model.ScanEventType]int{
        model.ScanEventScanStarted:      1,
        model.ScanEventFileScanStarted:  2,
        model.ScanEventFileScanFinished: 2,
        model.ScanEventScanFinished:     1,
    }
    receivedEvents := make(map[model.ScanEventType]int)
    fileStart := map[string]bool{"a": false, "b": false}
    fileFinish := map[string]bool{"a": false, "b": false}

    verdicts := map[string]string{} // file name -> verdict

    go func() {
        for {
            select {
            case evt := <-eventsCh:
                receivedEvents[evt.Status]++
                switch evt.Status {
                case model.ScanEventFileScanStarted:
                    fileStart[evt.File.FileId] = true
                case model.ScanEventFileScanFinished:
                    fileFinish[evt.File.FileId] = true
                }
            case res := <-resultsCh:
                for _, f := range res.Files {
                    verdicts[f.Name] = f.Verdict
                }
            case <-ctx.Done():
                close(done)
                return
            }

            // Check if all expectations met
            allEvents := true
            for k, v := range expectedEvents {
                if receivedEvents[k] < v {
                    allEvents = false
                    break
                }
            }
            filesDone := fileStart["a"] && fileStart["b"] && fileFinish["a"] && fileFinish["b"]
            twoVerdicts := len(verdicts) >= 2
            if allEvents && filesDone && twoVerdicts {
                close(done)
                return
            }
        }
    }()

    <-done

    // Stop workers and consumers gracefully
    cancel()
    wp.Wait()

    // Assertions
    if got := receivedEvents[model.ScanEventScanStarted]; got != 1 {
        t.Fatalf("expected 1 scan_started event, got %d", got)
    }
    if got := receivedEvents[model.ScanEventScanFinished]; got != 1 {
        t.Fatalf("expected 1 scan_finished event, got %d", got)
    }
    if got := receivedEvents[model.ScanEventFileScanStarted]; got < 2 {
        t.Fatalf("expected 2 file_scan_started events, got %d", got)
    }
    if got := receivedEvents[model.ScanEventFileScanFinished]; got < 2 {
        t.Fatalf("expected 2 file_scan_finished events, got %d", got)
    }
    if !fileStart["a"] || !fileStart["b"] || !fileFinish["a"] || !fileFinish["b"] {
        t.Fatalf("expected per-file start/finish events for files a and b, got start=%v finish=%v", fileStart, fileFinish)
    }

    // Result verdicts: eicar.txt FOUND, http_request.http OK
    if v, ok := verdicts["eicar.txt"]; !ok || v != "FOUND" {
        t.Fatalf("expected verdict for eicar.txt = FOUND, got %q (present=%v)", v, ok)
    }
    if v, ok := verdicts["http_request.http"]; !ok || v != "OK" {
        t.Fatalf("expected verdict for http_request.http = OK, got %q (present=%v)", v, ok)
    }
}

func startMQ(err error, ctx context.Context, rabbitMqVersion string) *testcontainers.DockerContainer {
    mq, err := testcontainers.Run(ctx, "docker.io/library/rabbitmq:"+rabbitMqVersion,
        testcontainers.WithExposedPorts("5672/tcp"),
        testcontainers.WithWaitStrategy(
            wait.ForListeningPort("5672/tcp"),
            wait.ForLog("Server startup complete"),
        ),
    )
    return mq
}

func startClamD(ctx context.Context, clamdVersion string) (*testcontainers.DockerContainer, error) {
    clamd, err := testcontainers.Run(ctx, "docker.io/clamav/clamav:"+clamdVersion,
        testcontainers.WithExposedPorts("3310/tcp"),
        testcontainers.WithWaitStrategy(
            wait.ForListeningPort("3310/tcp"),
            wait.ForLog("socket found, clamd started."),
        ),
        testcontainers.WithFiles(
            testcontainers.ContainerFile{
                HostFilePath:      "/home/kubenetic/Workspace/kubenetic/ClamGo/test/eicar.txt",
                ContainerFilePath: "/testfiles/eicar.txt",
                FileMode:          0o644,
            },
            testcontainers.ContainerFile{
                HostFilePath:      "/home/kubenetic/Workspace/kubenetic/ClamGo/test/scan_request.http",
                ContainerFilePath: "/testfiles/scan_request.http",
                FileMode:          0o644,
            },
        ),
    )
    return clamd, err
}

func initMQ(t *testing.T, mqAddr string) {
    rabbitConn, err := amqp091.Dial(mqAddr)
    if err != nil {
        t.Fatal(err)
    }
    defer rabbitConn.Close()
    log.Info().Msg("RabbitMQ connection established")

    adminCh, err := rabbitConn.Channel()
    if err != nil {
        t.Fatal(err)
    }
    defer adminCh.Close()
    log.Info().Msg("RabbitMQ admin channel established")

    const (
        jobsExchange       = "scan.jobs.x"
        retryExchange      = "scan.retry.x"
        resultsExchange    = "scan.results.x"
        eventsExchange     = "scan.events.x"
        deadLetterExchange = "scan.dead.x"

        jobsQueue       = "scan.jobs.q"
        eventsQueue     = "scan.events.q"
        resultsQueue    = "scan.results.q"
        retryQueue30s   = "scan.retry.30s.q"
        retryQueue1m    = "scan.retry.1m.q"
        retryQueue2m    = "scan.retry.2m.q"
        deadLetterQueue = "scan.dead.q"
    )

    _ = adminCh.ExchangeDeclare(jobsExchange, amqp091.ExchangeDirect, true, false, false, false, nil)
    _ = adminCh.ExchangeDeclare(retryExchange, amqp091.ExchangeDirect, true, false, false, false, nil)
    _ = adminCh.ExchangeDeclare(eventsExchange, amqp091.ExchangeDirect, true, false, false, false, nil)
    _ = adminCh.ExchangeDeclare(resultsExchange, amqp091.ExchangeDirect, true, false, false, false, nil)
    _ = adminCh.ExchangeDeclare(deadLetterExchange, amqp091.ExchangeDirect, true, false, false, false, nil)
    log.Info().Msg("RabbitMQ exchanges declared")

    jobsQ, _ := adminCh.QueueDeclare(jobsQueue, true, false, false, false, amqp091.Table{
        "x-dead-letter-exchange":    "scan.dead.x",
        "x-dead-letter-routing-key": "dead",
    })
    eventsQ, _ := adminCh.QueueDeclare(eventsQueue, true, false, false, false, amqp091.Table{
        "x-dead-letter-exchange":    "scan.dead.x",
        "x-dead-letter-routing-key": "dead",
    })
    resultsQ, _ := adminCh.QueueDeclare(resultsQueue, true, false, false, false, amqp091.Table{
        "x-dead-letter-exchange":    "scan.dead.x",
        "x-dead-letter-routing-key": "dead",
    })
    retryQ30s, _ := adminCh.QueueDeclare(retryQueue30s, true, false, false, false, amqp091.Table{
        "x-dead-letter-exchange":    "scan.jobs.x",
        "x-dead-letter-routing-key": "30s",
        "x-message-ttl":             30000,
    })
    retryQ1m, _ := adminCh.QueueDeclare(retryQueue1m, true, false, false, false, amqp091.Table{
        "x-dead-letter-exchange":    "scan.jobs.x",
        "x-dead-letter-routing-key": "1m",
        "x-message-ttl":             60000,
    })
    retryQ2m, _ := adminCh.QueueDeclare(retryQueue2m, true, false, false, false, amqp091.Table{
        "x-dead-letter-exchange":    "scan.jobs.x",
        "x-dead-letter-routing-key": "2m",
        "x-message-ttl":             120000,
    })
    dlq, _ := adminCh.QueueDeclare(deadLetterQueue, true, false, false, false, nil)
    log.Info().Msg("RabbitMQ queues declared")

    _ = adminCh.QueueBind(jobsQ.Name, "jobs", jobsExchange, false, nil)
    _ = adminCh.QueueBind(eventsQ.Name, "event", eventsExchange, false, nil)
    _ = adminCh.QueueBind(resultsQ.Name, "result", resultsExchange, false, nil)
    _ = adminCh.QueueBind(retryQ30s.Name, "30s", retryExchange, false, nil)
    _ = adminCh.QueueBind(retryQ1m.Name, "1m", retryExchange, false, nil)
    _ = adminCh.QueueBind(retryQ2m.Name, "2m", retryExchange, false, nil)
    _ = adminCh.QueueBind(dlq.Name, "dead", deadLetterExchange, false, nil)
    log.Info().Msg("RabbitMQ queues bound")
}
