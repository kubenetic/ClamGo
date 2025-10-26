package scanner

import (
    "context"
    "errors"
    "fmt"
    "os"
    "strings"
    "sync"
    "time"

    "ClamGo/internal/rabbitmq"
    "ClamGo/pkg/model"
    "ClamGo/pkg/service/clamd"

    "github.com/rabbitmq/amqp091-go"
    "github.com/rs/zerolog/log"
)

const xScanJobs = "scan.jobs.x"
const xScanResults = "scan.results.x"
const xScanRetry = "scan.retry.x"
const xScanEvents = "scan.events.x"
const xScanDead = "scan.dead.x"

const qScanJobs = "scan.jobs.q"
const qScanResults = "scan.results.q"
const qScanRetry = "scan.retry.q"
const qScanEvents = "scan.events.q"
const qScanDead = "scan.dead.q"

var ErrMaxAttemptsReached = errors.New("max attempts reached")
var ErrScanRetry = errors.New("scan failed, retrying")
var ErrEmptyScanJob = errors.New("no files provided for scan")

type WorkerPool struct {
    mqConn *rabbitmq.ConnectionManager
    wg     sync.WaitGroup
}

func (wp *WorkerPool) Wait() {
    wp.wg.Wait()
}

func nonBlockErr(err error, errCh chan<- error) {
    select {
    case errCh <- err:
    default:
    }
}

func genConsumerTag(wid int) string {
    hostname, err := os.Hostname()
    if err != nil {
        hostname = "unknown"
    }

    return fmt.Sprintf("c-%s-%d-%d", hostname, wid, os.Getpid())
}

func (wp *WorkerPool) Run(ctx context.Context, workerCount int) <-chan error {
    log.Info().
        Int("workerCount", workerCount).
        Msg("starting worker pool")

    if workerCount < 1 {
        errCh := make(chan error, 1)
        nonBlockErr(fmt.Errorf("worker count must be greater than 0"), errCh)
        close(errCh)

        return errCh
    }

    errCh := make(chan error)

    for wid := 0; wid < workerCount; wid++ {
        wp.wg.Go(func() {
            log.Debug().Int("worker", wid).Msg("starting worker")

            // Create a new context for each worker
            wCtx, wCncl := context.WithCancel(ctx)
            defer wCncl()

            consumer, err := rabbitmq.NewConsumer(wp.mqConn)
            if err != nil {
                nonBlockErr(fmt.Errorf("cannot create rabbitmq consumer. %w", err), errCh)
                return
            }
            log.Debug().Int("worker", wid).Msg("worker has connected to rabbitmq")

            publisher, err := rabbitmq.NewPublisher(wp.mqConn)
            if err != nil {
                nonBlockErr(fmt.Errorf("cannot create rabbitmq publisher. %w", err), errCh)
                return
            }

            worker := Worker{
                id:        wid,
                publisher: publisher,
            }

            worker.OnScanStarted = func(ctx context.Context, jobId string) error {
                if err := worker.emitScanStartedEvent(ctx, jobId); err != nil {
                    return err
                }
                log.Trace().Int("worker", wid).Msg("emitted scan started event")
                return nil
            }

            worker.OnScanFinished = func(ctx context.Context, jobId string) error {
                if err := worker.emitScanFinishedEvent(ctx, jobId); err != nil {
                    return err
                }
                log.Trace().Int("worker", wid).Msg("scan finished, published scan event finished event")
                return nil
            }

            worker.OnFileScanStarted = func(ctx context.Context, jobId string, fileData model.RequestFileMeta) error {
                if err := worker.emitScanFileStartedEvent(ctx, jobId, fileData); err != nil {
                    return err
                }
                log.Trace().Int("worker", wid).Msg("emitted scan file started event")
                return nil
            }

            worker.OnFileScanFinished = func(ctx context.Context, jobId string, fileData model.RequestFileMeta) error {
                if err := worker.emitScanFileFinishedEvent(ctx, jobId, fileData); err != nil {
                    return err
                }
                log.Trace().Int("worker", wid).Msg("emitted scan file finished event")
                return nil
            }

            worker.OnScanFailed = func(ctx context.Context, jobId string, evtType model.ScanEventType, fileData model.RequestFileMeta, err error) error {
                if err := worker.emitScanFileFailedEvent(ctx, jobId, fileData, evtType, err); err != nil {
                    return err
                }
                log.Trace().Int("worker", wid).Msg("emitted scan file failed event")
                return nil
            }

            worker.OnPublishResults = func(ctx context.Context, jobId string, payload model.ScanResponse) error {
                if err := worker.publishResults(ctx, payload); err != nil {
                    return fmt.Errorf("error publishing results: %w", err)
                }
                log.Trace().Int("worker", wid).Msg("published scan results")
                return nil
            }

            err = consumer.Subscribe(wCtx, qScanJobs, genConsumerTag(wid), worker.handleScanCb)
            if err != nil {
                nonBlockErr(fmt.Errorf("cannot subscribe to rabbitmq queue. %w", err), errCh)
                return
            }
        })
    }

    return errCh
}

func handleClamdSession(jobId string, wid int, cb func(clamClient *clamd.ClamClient) (*ScanResult, error)) (*ScanResult, error) {
    // TODO: maintain a connection pool of ClamD and do not open a new connection for each scan
    // Connect to ClamD
    clamClient, err := clamd.NewClamClient()
    if err != nil {
        log.Error().
            Err(err).
            Str("jobId", jobId).
            Int("worker", wid).
            Msg("error connecting to clamd, republishing message")

        return nil, fmt.Errorf("clamd: error connecting to clamd: %w", err)
    }
    defer clamClient.Close()
    log.Debug().Int("worker", wid).Msg("worker has connected to clamd")

    if err := clamClient.OpenSession(); err != nil {
        log.Error().
            Err(err).
            Str("jobId", jobId).
            Int("worker", wid).
            Msg("error open new clamd session, republishing message")

        return nil, fmt.Errorf("clamd: error opening session: %w", err)
    }
    defer clamClient.CloseSession()
    log.Trace().Int("worker", wid).Msg("opened session")

    return cb(clamClient)
}

func (w *Worker) handleScanCb(ctx context.Context, message amqp091.Delivery) *rabbitmq.ConsumeError {
    log.Trace().Int("worker", w.id).Msg("received message")

    req, err := decodeScanRequest(&message)
    if err != nil {
        return rabbitmq.NewConsumeError(fmt.Errorf("error decoding message: %w", err))
    }

    if len(req.Files) == 0 {
        log.Warn().Int("worker", w.id).Msg("received scan request with no files")
        if err := w.OnScanFailed(ctx, req.JobId, model.ScanEventFileScanFailed, model.RequestFileMeta{}, ErrEmptyScanJob); err != nil {
            log.Error().Err(err).Int("worker", w.id).Msg("error emitting scan failed event")
            return rabbitmq.NewConsumeError(fmt.Errorf("error emitting scan failed event: %w", err))
        }

        return rabbitmq.NewConsumeError(message.Nack(false, false))
    }

    if err := w.OnScanStarted(ctx, req.JobId); err != nil {
        log.Error().
            Err(err).
            Str("jobId", req.JobId).
            Int("worker", w.id).
            Msg("error emitting scan started event")
    }

    log.Trace().Int("worker", w.id).Msg("decoded message")
    scanStarted := time.Now()

    scanResult, err := handleClamdSession(req.JobId, w.id, func(client *clamd.ClamClient) (*ScanResult, error) {
        return w.scanFiles(ctx, client, req)
    })

    if err != nil {
        if errors.Is(err, ErrMaxAttemptsReached) {
            log.Warn().
                Err(err).
                Str("jobId", req.JobId).
                Int("worker", w.id).
                Msg("max attempts reached, put message to dead letter queue")

            return rabbitmq.NewConsumeError(message.Nack(false, false))
        }

        if errors.Is(err, ErrScanRetry) {
            if scanResult == nil || scanResult.Retry == nil {
                return rabbitmq.NewConsumeError(fmt.Errorf("got retry error, retry payload is nil"))
            }

            if err := message.Ack(false); err != nil {
                return rabbitmq.NewConsumeError(fmt.Errorf("error acking message: %w", err))
            }

            return nil
        }

        if strings.HasPrefix(err.Error(), "clamd: ") {
            log.Error().
                Err(err).
                Str("jobId", req.JobId).
                Int("worker", w.id).
                Msg("error scanning files, republishing message")
            return rabbitmq.NewConsumeError(err, rabbitmq.WithRepublish())
        }

        return rabbitmq.NewConsumeError(err)
    }

    scanResponse := scanResult.Response
    scanResponse.Elapsed = time.Since(scanStarted).Milliseconds()
    scanResponse.JobId = req.JobId
    scanResponse.Timestamp = time.Now()

    log.Trace().Int("worker", w.id).Msg("finished scanning files")

    if err := w.OnPublishResults(ctx, req.JobId, scanResponse); err != nil {
        return rabbitmq.NewConsumeError(fmt.Errorf("error publishing results: %w", err))
    }

    if err := message.Ack(false); err != nil {
        return rabbitmq.NewConsumeError(fmt.Errorf("error acknowledging message: %w", err))
    }
    log.Trace().Int("worker", w.id).Msg("acknowledged message")

    if err := w.OnScanFinished(ctx, req.JobId); err != nil {
        log.Error().
            Err(err).
            Str("jobId", req.JobId).
            Int("worker", w.id).
            Msg("error emitting scan finish event")
    }

    return nil
}
