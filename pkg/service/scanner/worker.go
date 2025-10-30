package scanner

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "strings"
    "time"

    "ClamGo/pkg/model"
    "ClamGo/pkg/service/clamd"

    mq_model "github.com/kubenetic/BunnyShepherd/pkg/model"
    "github.com/kubenetic/BunnyShepherd/pkg/rabbitmq"
    amqp "github.com/rabbitmq/amqp091-go"
    "github.com/rs/zerolog/log"
)

const (
    maxScanAttempts = 3
)

type Worker struct {
    id        int
    publisher *rabbitmq.Publisher

    OnScanStarted      func(ctx context.Context, jobId string) error
    OnScanFinished     func(ctx context.Context, jobId string) error
    OnFileScanStarted  func(ctx context.Context, jobId string, fileData model.RequestFileMeta) error
    OnFileScanFinished func(ctx context.Context, jobId string, fileData model.RequestFileMeta) error
    OnScanFailed       func(ctx context.Context, jobId string, evtType model.ScanEventType, fileData model.RequestFileMeta, err error) error
    OnPublishResults   func(ctx context.Context, jobId string, payload model.ScanResponse) error
}

type ScanResult struct {
    Response model.ScanResponse
    Retry    *model.ScanRequest
}

func decodeScanRequest(msg *amqp.Delivery) (model.ScanRequest, error) {
    var req model.ScanRequest
    if err := json.Unmarshal(msg.Body, &req); err != nil {
        return req, fmt.Errorf("error unmarshalling message: %w", err)
    }
    return req, nil
}

func (w *Worker) scanFiles(ctx context.Context, clamClient *clamd.ClamClient, req model.ScanRequest) (*ScanResult, error) {
    var erroredFiles []model.RequestFileMeta
    res := model.ScanResponse{
        Files: make([]model.ResponseFileMeta, 0, len(req.Files)),
    }

    for _, fileData := range req.Files {
        select {
        case <-ctx.Done():
            _ = w.OnScanFailed(ctx, req.JobId, model.ScanEventScanInterrupted, fileData, ctx.Err())
            return nil, ctx.Err()
        default:
        }

        fileScanStarted := time.Now()

        if err := w.OnFileScanStarted(ctx, req.JobId, fileData); err != nil {
            return nil, fmt.Errorf("error emitting scan file started event: %w", err)
        }

        log.Trace().
            Int("worker", w.id).
            Str("jobId", req.JobId).
            Str("file", fileData.Name).
            Msg("started scanning file")

        finding, err := clamClient.ScanFile(fileData.Path)
        if err != nil && errors.Is(err, clamd.ErrFileNotFound) {
            log.Warn().Err(err).
                Int("worker", w.id).
                Str("jobId", req.JobId).
                Str("file", fileData.Name).
                Str("path", fileData.Path).
                Msg("file not found")

            if err := w.OnScanFailed(ctx, req.JobId, model.ScanEventFileScanFailed, fileData, err); err != nil {
                return nil, fmt.Errorf("error emitting scan failed event: %w", err)
            }

            continue
        } else if err != nil {
            if req.Attempts > maxScanAttempts {
                log.Warn().Err(err).
                    Int("worker", w.id).
                    Str("jobId", req.JobId).
                    Str("file", fileData.Name).
                    Str("path", fileData.Path).
                    Msg("failed to scan file, max attempts reached")

                if err := w.OnScanFailed(ctx, req.JobId, model.ScanEventFileMaxAttemptsReached, fileData, err); err != nil {
                    return nil, fmt.Errorf("error emitting scan file max attempts reached event: %w", err)
                }

                return nil, ErrMaxAttemptsReached
            }

            log.Trace().
                Err(err).
                Int("worker", w.id).
                Str("jobId", req.JobId).
                Str("file", fileData.Name).
                Int("attempts", req.Attempts).
                Msg("failed to scan file, retrying")

            erroredFiles = append(erroredFiles, fileData)
            continue
        }

        verdict := "OK"
        if finding != "OK" {
            verdict = "FOUND"
        }

        log.Trace().
            Int("worker", w.id).
            Str("jobId", req.JobId).
            Str("file", fileData.Name).
            Str("verdict", verdict).
            Str("finding", finding).
            Msg("finished scanning file")

        res.Files = append(res.Files, model.ResponseFileMeta{
            FileId:    fileData.FileId,
            Name:      fileData.Name,
            Sha256:    fileData.Sha256,
            Size:      fileData.Size,
            Verdict:   verdict,
            Signature: finding,
            Elapsed:   time.Since(fileScanStarted).Milliseconds(),
        })

        if err := w.OnFileScanFinished(ctx, req.JobId, fileData); err != nil {
            return nil, fmt.Errorf("error emitting scan file finished event: %w", err)
        }

        log.Trace().
            Int("worker", w.id).
            Str("jobId", req.JobId).
            Str("file", fileData.Name).
            Msg("published scan event finished event")
    }

    if len(erroredFiles) > 0 {
        retryReq := model.ScanRequest{
            JobId:     req.JobId,
            Attempts:  req.Attempts + 1,
            Timestamp: time.Now(),
            Files:     erroredFiles,
        }

        return &ScanResult{
            Response: model.ScanResponse{},
            Retry:    &retryReq,
        }, ErrScanRetry
    }

    return &ScanResult{
        Response: res,
        Retry:    nil,
    }, nil
}

func (w *Worker) emitScanEvent(ctx context.Context, evtType model.ScanEventType, jobId string, fileMeta model.ScanEventFileMetadata) error {
    message := &mq_model.JSONMessage[model.ScanEvent]{
        Payload: model.ScanEvent{
            JobId:     jobId,
            Timestamp: time.Now(),
            File:      fileMeta,
            Status:    evtType,
        },
        MessageId:     "",
        CorrelationId: "",
        Headers:       nil,
    }
    return w.publisher.Publish(ctx, xScanEvents, "event", false, message)
}

func (w *Worker) emitScanStartedEvent(ctx context.Context, jobId string) error {
    return w.emitScanEvent(ctx, model.ScanEventScanStarted, jobId, model.ScanEventFileMetadata{})
}

func (w *Worker) emitScanFileStartedEvent(ctx context.Context, jobId string, fileMeta model.RequestFileMeta) error {
    return w.emitScanEvent(ctx, model.ScanEventFileScanStarted, jobId, fileMeta.ConvertFileMeta())
}

func (w *Worker) emitScanFileFinishedEvent(ctx context.Context, jobId string, fileMeta model.RequestFileMeta) error {
    return w.emitScanEvent(ctx, model.ScanEventFileScanFinished, jobId, fileMeta.ConvertFileMeta())
}

func (w *Worker) emitScanFinishedEvent(ctx context.Context, jobId string) error {
    return w.emitScanEvent(ctx, model.ScanEventScanFinished, jobId, model.ScanEventFileMetadata{})
}

func (w *Worker) emitScanFileFailedEvent(ctx context.Context, jobId string, fileMeta model.RequestFileMeta, evtType model.ScanEventType, cause error) error {
    message := &mq_model.JSONMessage[model.ScanEvent]{
        Payload: model.ScanEvent{
            JobId:     jobId,
            Timestamp: time.Now(),
            File:      fileMeta.ConvertFileMeta(),
            Status:    evtType,
            Error:     cause.Error(),
        },
        MessageId:     "",
        CorrelationId: "",
        Headers:       nil,
    }
    return w.publisher.Publish(ctx, xScanEvents, "event", false, message)
}

func (w *Worker) publishResults(ctx context.Context, payload model.ScanResponse) error {
    message := &mq_model.JSONMessage[model.ScanResponse]{
        Payload:       payload,
        MessageId:     "",
        CorrelationId: "",
        Headers:       nil,
        ContentType:   "",
    }

    return w.publisher.Publish(ctx, xScanResults, "result", false, message)
}

func (w *Worker) handleScanCb(ctx context.Context, message amqp.Delivery) error {

    req, err := decodeScanRequest(&message)
    if err != nil {
        return fmt.Errorf("error decoding message: %w", err)
    }

    log.Trace().
        Int("worker", w.id).
        Str("jobId", req.JobId).
        Msg("received message")

    if len(req.Files) == 0 {
        log.Warn().
            Int("worker", w.id).
            Str("jobId", req.JobId).
            Msg("received scan request with no files")

        if err := w.OnScanFailed(ctx, req.JobId, model.ScanEventFileScanFailed, model.RequestFileMeta{}, ErrEmptyScanJob); err != nil {
            log.Error().
                Err(err).
                Int("worker", w.id).
                Str("jobId", req.JobId).
                Msg("error emitting scan failed event")
            return fmt.Errorf("error emitting scan failed event: %w", err)
        }

        return message.Nack(false, false)
    }

    if err := w.OnScanStarted(ctx, req.JobId); err != nil {
        log.Error().
            Err(err).
            Str("jobId", req.JobId).
            Int("worker", w.id).
            Msg("error emitting scan started event")
    }

    log.Trace().
        Int("worker", w.id).
        Str("jobId", req.JobId).
        Msg("decoded message")
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

            return message.Nack(false, false)
        }

        if errors.Is(err, ErrScanRetry) {
            if scanResult == nil || scanResult.Retry == nil {
                return fmt.Errorf("got retry error, retry payload is nil")
            }

            if err := w.republish(ctx, message, *scanResult.Retry); err != nil {
                if errors.Is(err, ErrMaxAttemptsReached) {
                    return message.Nack(false, false)
                }
                return fmt.Errorf("error republishing message: %w", err)
            }

            if err := message.Ack(false); err != nil {
                return fmt.Errorf("error acking message: %w", err)
            }

            return nil
        }

        if strings.HasPrefix(err.Error(), "clamd: ") {
            log.Error().
                Err(err).
                Str("jobId", req.JobId).
                Int("worker", w.id).
                Msg("error scanning files, republishing message")
            return err
        }

        return err
    }

    scanResponse := scanResult.Response
    scanResponse.Elapsed = time.Since(scanStarted).Milliseconds()
    scanResponse.JobId = req.JobId
    scanResponse.Timestamp = time.Now()

    log.Trace().
        Int("worker", w.id).
        Str("jobId", req.JobId).
        Msg("finished scanning files")

    if err := w.OnPublishResults(ctx, req.JobId, scanResponse); err != nil {
        return fmt.Errorf("error publishing results: %w", err)
    }

    if err := message.Ack(false); err != nil {
        return fmt.Errorf("error acknowledging message: %w", err)
    }
    log.Trace().
        Int("worker", w.id).
        Str("jobId", req.JobId).
        Msg("acknowledged message")

    if err := w.OnScanFinished(ctx, req.JobId); err != nil {
        log.Error().
            Err(err).
            Str("jobId", req.JobId).
            Int("worker", w.id).
            Msg("error emitting scan finish event")
    }

    return nil
}

func (w *Worker) republish(ctx context.Context, originalMsg amqp.Delivery, retryReq model.ScanRequest) error {
    attempt := getAttempts(originalMsg.Headers)
    exchange, routingKey := getMsgDestination(attempt)

    if attempt > 3 {
        return ErrMaxAttemptsReached
    } else {
        log.Warn().
            Str("jobId", retryReq.JobId).
            Int("attempt", attempt).
            Str("exchange", exchange).
            Str("routingKey", routingKey).
            Msg("message failed to process, try to republish")
    }

    repubCtx, repubCncl := context.WithTimeout(ctx, 30*time.Second)
    defer repubCncl()

    headers := cloneHeaders(originalMsg.Headers)
    headers["x-attempts"] = attempt + 1

    return w.publisher.Publish(
        repubCtx, exchange, routingKey, false, &mq_model.JSONMessage[model.ScanRequest]{
            Payload:       retryReq,
            MessageId:     originalMsg.MessageId,
            CorrelationId: originalMsg.CorrelationId,
            Headers:       headers,
            ContentType:   originalMsg.ContentType,
        })
}

func getMsgDestination(attempt int) (string, string) {
    switch attempt {
    case 1:
        return "scan.retry.x", "5s"
    case 2:
        return "scan.retry.x", "30s"
    case 3:
        return "scan.retry.x", "1m"
    default:
        return "scan.dead.x", "dead"
    }
}

func getAttempts(headers amqp.Table) int {
    attempt, ok := headers["x-attempts"]
    if !ok {
        return 1
    }
    return attempt.(int)
}

func cloneHeaders(h amqp.Table) amqp.Table {
    out := amqp.Table{}
    for k, v := range h {
        out[k] = v
    }
    return out
}
