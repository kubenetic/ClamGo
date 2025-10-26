package scanner

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "time"

    "ClamGo/internal/rabbitmq"
    "ClamGo/pkg/model"
    "ClamGo/pkg/service/clamd"

    "github.com/rabbitmq/amqp091-go"
    "github.com/rs/zerolog/log"
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

func decodeScanRequest(msg *amqp091.Delivery) (model.ScanRequest, error) {
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
            Str("file", fileData.Name).
            Msg("started scanning file")

        finding, err := clamClient.ScanFile(fileData.Path)
        if err != nil && errors.Is(err, clamd.ErrFileNotFound) {
            log.Warn().Err(err).
                Int("worker", w.id).
                Str("file", fileData.Name).
                Str("path", fileData.Path).
                Msg("file not found")

            if err := w.OnScanFailed(ctx, req.JobId, model.ScanEventFileScanFailed, fileData, err); err != nil {
                return nil, fmt.Errorf("error emitting scan failed event: %w", err)
            }

            continue
        } else if err != nil {
            if req.Attempts > 3 {
                log.Warn().Err(err).
                    Int("worker", w.id).
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
    message := &model.JSONMessage[model.ScanEvent]{
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
    message := &model.JSONMessage[model.ScanEvent]{
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
    message := &model.JSONMessage[model.ScanResponse]{
        Payload:       payload,
        MessageId:     "",
        CorrelationId: "",
        Headers:       nil,
        ContentType:   "",
    }

    return w.publisher.Publish(ctx, xScanResults, "result", false, message)
}
