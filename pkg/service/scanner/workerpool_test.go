package scanner

import (
    "context"
    "encoding/json"
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

// TODO: Move to rabbitmq/consumer_test.go
// func Test_getMsgDestination(t *testing.T) {
//     t.Parallel()
//
//     tests := []struct {
//         name             string
//         attempt          int
//         wantedExchange   string
//         wantedRoutingKey string
//     }{
//         {"Retry 1st time", 1, "scan.retry.x", "30s"},
//         {"Retry 2nd time", 2, "scan.retry.x", "1m"},
//         {"Retry 3rd time", 3, "scan.retry.x", "2m"},
//         {"Retry 4th time", 4, "scan.dead.x", "dead"},
//         {"Retry nth time", 5, "scan.dead.x", "dead"},
//     }
//
//     for _, tt := range tests {
//         t.Run(tt.name, func(t *testing.T) {
//             expectedExchange, expectedRoutingKey := getMsgDestination(tt.attempt)
//
//             if expectedExchange != tt.wantedExchange {
//                 t.Errorf("getMsgDestination(%d) got = %v, want %v", tt.attempt, tt.wantedExchange, expectedExchange)
//             }
//             if expectedRoutingKey != tt.wantedRoutingKey {
//                 t.Errorf("getMsgDestination(%d) got1 = %v, want %v", tt.attempt, tt.wantedRoutingKey, expectedRoutingKey)
//             }
//         })
//     }
// }

func Test_Run(t *testing.T) {
    t.Parallel()

    const (
        clamdVersion    = "1.5.1"
        rabbitMqVersion = "4.1"
        workerCount     = 3
    )

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

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

    rabbitConn, err := amqp091.Dial(mqAddr)
    if err != nil {
        t.Fatal(err)
    }
    defer rabbitConn.Close()
    log.Info().Msg("RabbitMQ connection established")

    mqCm, err := rabbitmq.NewConnectionManager(ctx, mqAddr, nil)
    if err != nil {
        t.Fatal(err)
    }
    defer mqCm.Close()
    log.Info().Msg("RabbitMQ connection manager established")

    wp := WorkerPool{
        mqConn: mqCm,
    }

    initMQ(t, rabbitConn)

    errCh := wp.Run(ctx, workerCount)
    go func() {
        for err := range errCh {
            t.Error(err)
        }
    }()
    log.Info().Msg("WorkerPool started")

    scanReq := model.ScanRequest{
        JobId:     "1234",
        Attempts:  1,
        Timestamp: time.Now(),
        Files: []model.RequestFileMeta{
            {
                FileId: "a",
                Name:   "eicar.txt",
                Path:   "/testfiles/eicar.txt",
            },
            {
                FileId: "b",
                Name:   "http_request.http",
                Path:   "/testfiles/scan_request.http",
            },
        },
    }

    pubCh, err := rabbitConn.Channel()
    if err != nil {
        t.Fatal(err)
    }
    defer pubCh.Close()
    log.Info().Msg("RabbitMQ publish channel established")

    scanReqJson, _ := json.Marshal(scanReq)
    err = pubCh.Publish("scan.jobs.x", "jobs", false, false, amqp091.Publishing{
        ContentType: "application/json",
        Timestamp:   time.Now(),
        Type:        "ScanRequest",
        Body:        scanReqJson,
    })
    if err != nil {
        t.Fatal(err)
    }
    defer func() {
        if err := pubCh.Close(); err != nil {
            log.Error().Err(err).Msg("error closing publish channel")
            t.Error(err)
        }
    }()
    log.Info().Msg("ScanRequest published")

    conCh, err := rabbitConn.Channel()
    if err != nil {
        t.Fatal(err)
    }
    defer func() {
        if err := conCh.Close(); err != nil {
            log.Error().Err(err).Msg("error closing consume channel")
            t.Error(err)
        }
    }()
    log.Info().Msg("RabbitMQ consume channel established")

    events, err := conCh.Consume("scan.events.q", "", true, false, false, false, nil)
    if err != nil {
        t.Fatal(err)
    }
    log.Info().Msg("event consumer started")

    for {
        log.Trace().Msg("waiting for events")

        select {
        case err, ok := <-errCh:
            if !ok {
                log.Error().Msg("error channel closed")
                t.Error("error channel closed")
                return
            }

            log.Error().Err(err).Msg("error received")
            t.Error(err)
            return

        case <-ctx.Done():
            log.Error().Msg("context cancelled")
            t.Error(ctx.Err())
            return

        case evt, ok := <-events:
            if !ok {
                log.Error().Msg("message channel closed")
                t.Error("message channel closed")
                return
            }

            _ = evt.Ack(false)

            var scanEvent model.ScanEvent
            if err := json.Unmarshal(evt.Body, &scanEvent); err != nil {
                log.Error().Err(err).Msg("error unmarshalling event")
                t.Error(err)
                return
            }

            if scanEvent.Status == model.ScanEventScanFinished {
                log.Info().
                    Str("jobID", scanEvent.JobId).
                    Msg("scan finished successfully")
                break
            }

            if scanEvent.Status == model.ScanEventFileScanFailed {
                log.Warn().
                    Str("jobID", scanEvent.JobId).
                    Msg("scan failed")
                break
            }

            if scanEvent.Status == model.ScanEventFileMaxAttemptsReached {
                log.Warn().
                    Str("jobID", scanEvent.JobId).
                    Msg("max attempts reached")
                break
            }

            log.Info().
                Str("jobID", scanEvent.JobId).
                Str("status", string(scanEvent.Status)).
                Msg("event consumed")
        }
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

func initMQ(t *testing.T, rabbitConn *amqp091.Connection) {
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
