# ClamGo RabbitMQ Message Schemas

This document defines all RabbitMQ message payloads used by ClamGo for inter-service communication.

## Message Types Overview

| Message Type | Queue | Exchange | Routing Key | Direction | Purpose |
|---|---|---|---|---|---|
| **FileUploadedMessage** | `q.file.scan` | `uploader.exchange` | (binding key) | Inbound | Trigger file scan |
| **ScanStartedMessage** | (fanout) | `uploader.exchange` | `file.scan.started` | Outbound | Scan has begun (first attempt only) |
| **ScanCompletedMessage** | (fanout) | `uploader.exchange` | `file.scan.completed` | Outbound | Successful scan result |
| **ScanRetryingMessage** | (fanout) | `uploader.exchange` | `file.scan.retrying` | Outbound | Scan failed, retrying |
| **ScanFailedMessage** | `scan.dead.q` | `uploader.dlx` | `file.scan.failed` | Outbound | Permanent scan failure |
| **CaseCancelledMessage** | `q.case.cancelled` | `uploader.exchange` | (binding key) | Inbound | Cancel case processing |

---

## 1. ScanStartedMessage (Outbound)

**Purpose**: Notifies the Java Backend that ClamGo has begun scanning a file (SSE push trigger)  
**Exchange**: `uploader.exchange`  
**Routing Key**: `file.scan.started`  
**Published**: Once per file, on the **first** scan attempt only (not on retries), after the file is confirmed open  
**Consumer**: Java Backend (SSE push to client)

### JSON Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "title": "ScanStartedMessage",
  "description": "Outbound message indicating ClamGo has started scanning a file",
  "required": ["fileId", "caseId", "originalName", "sizeBytes", "startedAt"],
  "properties": {
    "fileId": {
      "type": "string",
      "description": "Unique file identifier (UUID)",
      "example": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
    },
    "caseId": {
      "type": "string",
      "description": "Case identifier linking multiple files",
      "example": "c-2025-001234"
    },
    "originalName": {
      "type": "string",
      "description": "Original filename as uploaded by user",
      "example": "document.pdf"
    },
    "sizeBytes": {
      "type": "integer",
      "format": "int64",
      "description": "File size in bytes",
      "minimum": 0,
      "example": 2457600
    },
    "startedAt": {
      "type": "string",
      "format": "date-time",
      "description": "ISO 8601 UTC timestamp when scanning began",
      "example": "2025-03-01T12:00:00.500Z"
    }
  }
}
```

### Example Payload

```json
{
  "fileId": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
  "caseId": "c-2025-001234",
  "originalName": "report_2025.pdf",
  "sizeBytes": 2457600,
  "startedAt": "2025-03-01T12:00:00.500Z"
}
```

### Processing Notes

- **Publish Timing**: Published after `os.Open` succeeds, guaranteeing the file exists before the backend is notified
- **Retry Guard**: Only published when `retryCount == 0`; retried deliveries do **not** re-emit this event
- **Best Effort**: Publish failure is logged as a warning but does not abort the scan
- **SSE Use**: The Java Backend uses this event to push a "scanning…" status to the client UI

---

## 2. FileUploadedMessage (Inbound)

**Purpose**: Triggers a file scan in ClamGo  
**Queue**: `q.file.scan`  
**Exchange**: `uploader.exchange`  
**Source**: tusd-token-hook (after successful file upload)  
**Consumer**: ClamGo scanner

### JSON Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "title": "FileUploadedMessage",
  "description": "Inbound message to trigger ClamGo file scanning",
  "required": ["fileId", "caseId", "tempPath", "originalName", "sizeBytes", "contentType", "uploadedAt"],
  "properties": {
    "fileId": {
      "type": "string",
      "description": "Unique file identifier (UUID)",
      "example": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
    },
    "caseId": {
      "type": "string",
      "description": "Case identifier linking multiple files",
      "example": "c-2025-001234"
    },
    "tempPath": {
      "type": "string",
      "description": "Absolute path to temporary file (must start with configured tempNFS.prefix)",
      "example": "/mnt/temp-nfs/uploads/f47ac10b-58cc-4372-a567-0e02b2c3d479"
    },
    "originalName": {
      "type": "string",
      "description": "Original filename as uploaded by user",
      "example": "document.pdf"
    },
    "sizeBytes": {
      "type": "integer",
      "format": "int64",
      "description": "File size in bytes",
      "minimum": 0,
      "example": 1024000
    },
    "contentType": {
      "type": "string",
      "description": "MIME type claimed during upload",
      "example": "application/pdf"
    },
    "uploadedAt": {
      "type": "string",
      "format": "date-time",
      "description": "ISO 8601 timestamp of upload completion",
      "example": "2025-03-01T12:00:00Z"
    }
  }
}
```

### Example Payload

```json
{
  "fileId": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
  "caseId": "c-2025-001234",
  "tempPath": "/mnt/temp-nfs/uploads/f47ac10b-58cc-4372-a567-0e02b2c3d479",
  "originalName": "report_2025.pdf",
  "sizeBytes": 2457600,
  "contentType": "application/pdf",
  "uploadedAt": "2025-03-01T12:00:00Z"
}
```

### Processing Notes

- **Validation**: Path must start with configured `tempNFS.prefix` (default: `/mnt/temp-nfs/`)
- **Cancellation Check**: If case is already cancelled, message is discarded (ACKed)
- **Error Handling**: File not found errors result in message ACK (no retry)
- **Retry Behavior**: Transient errors (I/O, clamd unavailable) trigger retry

---

## 3. ScanCompletedMessage (Outbound)

**Purpose**: Reports successful file scan completion (clean or infected)  
**Exchange**: `uploader.exchange`  
**Routing Key**: `file.scan.completed`  
**Published After**: Successful scan completion or infection detection  
**Consumer**: Java Backend job tracker

### JSON Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "title": "ScanCompletedMessage",
  "description": "Outbound message reporting scan completion (success or infection)",
  "required": ["fileId", "caseId", "verdict", "checksumSha256", "magicByteAnalysis", "engineVersion", "signatureVersion", "scannedAt", "scanDurationMs"],
  "properties": {
    "fileId": {
      "type": "string",
      "description": "Unique file identifier (matches request)",
      "example": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
    },
    "caseId": {
      "type": "string",
      "description": "Case identifier (matches request)",
      "example": "c-2025-001234"
    },
    "verdict": {
      "type": "string",
      "enum": ["CLEAN", "INFECTED", "ERROR"],
      "description": "Scan verdict: CLEAN = no malware, INFECTED = threat detected, ERROR = scan error",
      "example": "CLEAN"
    },
    "threatName": {
      "type": "string",
      "description": "ClamAV threat signature name (only present if INFECTED)",
      "example": "Trojan.Generic.12345"
    },
    "checksumSha256": {
      "type": "string",
      "pattern": "^[a-f0-9]{64}$",
      "description": "SHA-256 hash of file content (lowercase hex)",
      "example": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    },
    "magicByteAnalysis": {
      "type": "object",
      "description": "File type consistency analysis",
      "required": ["detectedMimeType", "claimedMimeType", "claimedExtension", "consistency"],
      "properties": {
        "detectedMimeType": {
          "type": "string",
          "description": "MIME type detected from file magic bytes",
          "example": "application/pdf"
        },
        "claimedMimeType": {
          "type": "string",
          "description": "MIME type claimed during upload",
          "example": "application/pdf"
        },
        "claimedExtension": {
          "type": "string",
          "description": "File extension extracted from filename",
          "example": ".pdf"
        },
        "consistency": {
          "type": "string",
          "enum": ["CONSISTENT", "MINOR_MISMATCH", "MISMATCH", "UNKNOWN", "EMPTY"],
          "description": "Consistency between detected and claimed type",
          "example": "CONSISTENT"
        },
        "note": {
          "type": "string",
          "description": "Optional explanatory note (e.g., for MISMATCH)",
          "example": ""
        }
      }
    },
    "engineVersion": {
      "type": "string",
      "description": "ClamAV engine version",
      "example": "1.0.0"
    },
    "signatureVersion": {
      "type": "string",
      "description": "ClamAV virus signature database version",
      "example": "27018/Wed Mar  1 02:13:00 2025"
    },
    "scannedAt": {
      "type": "string",
      "format": "date-time",
      "description": "ISO 8601 timestamp of scan completion",
      "example": "2025-03-01T12:00:01.234Z"
    },
    "scanDurationMs": {
      "type": "integer",
      "format": "int64",
      "description": "Total scan duration in milliseconds",
      "minimum": 0,
      "example": 1234
    }
  }
}
```

### Example Payloads

#### Clean File

```json
{
  "fileId": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
  "caseId": "c-2025-001234",
  "verdict": "CLEAN",
  "checksumSha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "magicByteAnalysis": {
    "detectedMimeType": "application/pdf",
    "claimedMimeType": "application/pdf",
    "claimedExtension": ".pdf",
    "consistency": "CONSISTENT"
  },
  "engineVersion": "1.0.0",
  "signatureVersion": "27018/Wed Mar  1 02:13:00 2025",
  "scannedAt": "2025-03-01T12:00:01.234Z",
  "scanDurationMs": 1234
}
```

#### Infected File

```json
{
  "fileId": "f47ac10b-58cc-4372-a567-0e02b2c3d480",
  "caseId": "c-2025-001234",
  "verdict": "INFECTED",
  "threatName": "Trojan.Generic.12345",
  "checksumSha256": "d41d8cd98f00b204e9800998ecf8427e0000000000000000000000000000000a",
  "magicByteAnalysis": {
    "detectedMimeType": "application/x-executable",
    "claimedMimeType": "text/plain",
    "claimedExtension": ".txt",
    "consistency": "MISMATCH",
    "note": "File claims to be text but contains executable code"
  },
  "engineVersion": "1.0.0",
  "signatureVersion": "27018/Wed Mar  1 02:13:00 2025",
  "scannedAt": "2025-03-01T12:00:02.456Z",
  "scanDurationMs": 2000
}
```

---

## 4. ScanRetryingMessage (Outbound)

**Purpose**: Notifies backend of scan failure and retry attempt  
**Exchange**: `uploader.exchange`  
**Routing Key**: `file.scan.retrying`  
**Published**: Each time a scan fails and is scheduled for retry  
**Consumer**: Java Backend job tracker

### JSON Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "title": "ScanRetryingMessage",
  "description": "Outbound message reporting scan failure and retry scheduling",
  "required": ["fileId", "caseId", "retryAttempt", "maxRetries", "error", "message", "failedAt"],
  "properties": {
    "fileId": {
      "type": "string",
      "description": "Unique file identifier",
      "example": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
    },
    "caseId": {
      "type": "string",
      "description": "Case identifier",
      "example": "c-2025-001234"
    },
    "retryAttempt": {
      "type": "integer",
      "description": "Current retry attempt number (1-3)",
      "minimum": 1,
      "maximum": 3,
      "example": 1
    },
    "maxRetries": {
      "type": "integer",
      "description": "Maximum number of retries",
      "example": 3
    },
    "error": {
      "type": "string",
      "description": "Error code or category",
      "example": "CLAMD_UNAVAILABLE"
    },
    "message": {
      "type": "string",
      "description": "Human-readable error message",
      "example": "Failed to connect to clamd at localhost:3310"
    },
    "nextRetryQueue": {
      "type": "string",
      "description": "Queue name where retry will be processed",
      "example": "q.file.scan.retry-1"
    },
    "nextRetryDelayMs": {
      "type": "integer",
      "format": "int64",
      "description": "Delay before retry in milliseconds",
      "example": 30000
    },
    "failedAt": {
      "type": "string",
      "format": "date-time",
      "description": "ISO 8601 timestamp of failure",
      "example": "2025-03-01T12:00:01.234Z"
    }
  }
}
```

### Example Payload

```json
{
  "fileId": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
  "caseId": "c-2025-001234",
  "retryAttempt": 1,
  "maxRetries": 3,
  "error": "CLAMD_UNAVAILABLE",
  "message": "Failed to connect to clamd at localhost:3310: connection refused",
  "nextRetryQueue": "q.file.scan.retry-1",
  "nextRetryDelayMs": 30000,
  "failedAt": "2025-03-01T12:00:01.234Z"
}
```

### Retry Schedule

| Attempt | Delay | Queue |
|---------|-------|-------|
| 1 | 30 seconds | `q.file.scan.retry-1` |
| 2 | 120 seconds | `q.file.scan.retry-2` |
| 3 | 600 seconds | `q.file.scan.retry-3` |

---

## 5. ScanFailedMessage (Outbound)

**Purpose**: Reports permanent scan failure after all retries exhausted  
**Exchange**: `uploader.dlx` (Dead Letter Exchange)  
**Routing Key**: `file.scan.failed`  
**Published**: After 3 failed retry attempts  
**Consumer**: Manual investigation/monitoring

### JSON Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "title": "ScanFailedMessage",
  "description": "Outbound message reporting permanent scan failure (all retries exhausted)",
  "required": ["fileId", "caseId", "error", "message", "retryCount", "retryHistory", "originalMessage", "failedAt"],
  "properties": {
    "fileId": {
      "type": "string",
      "description": "Unique file identifier",
      "example": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
    },
    "caseId": {
      "type": "string",
      "description": "Case identifier",
      "example": "c-2025-001234"
    },
    "error": {
      "type": "string",
      "description": "Final error code",
      "example": "CLAMD_UNAVAILABLE"
    },
    "message": {
      "type": "string",
      "description": "Final error message",
      "example": "Clamd remained unavailable after 3 retry attempts"
    },
    "retryCount": {
      "type": "integer",
      "description": "Total number of retry attempts made",
      "example": 3
    },
    "retryHistory": {
      "type": "array",
      "description": "History of all failed attempts",
      "items": {
        "type": "object",
        "required": ["attempt", "error", "failedAt", "retryQueue"],
        "properties": {
          "attempt": {
            "type": "integer",
            "description": "Retry attempt number",
            "example": 1
          },
          "error": {
            "type": "string",
            "description": "Error at this attempt",
            "example": "CLAMD_UNAVAILABLE"
          },
          "failedAt": {
            "type": "string",
            "format": "date-time",
            "description": "ISO 8601 timestamp",
            "example": "2025-03-01T12:00:01.234Z"
          },
          "retryQueue": {
            "type": "string",
            "description": "Queue used for this retry",
            "example": "q.file.scan.retry-1"
          }
        }
      }
    },
    "originalMessage": {
      "type": "object",
      "description": "Original FileUploadedMessage that triggered the scan",
      "properties": {
        "fileId": {"type": "string"},
        "caseId": {"type": "string"},
        "tempPath": {"type": "string"},
        "originalName": {"type": "string"},
        "sizeBytes": {"type": "integer"},
        "contentType": {"type": "string"},
        "uploadedAt": {"type": "string", "format": "date-time"}
      }
    },
    "failedAt": {
      "type": "string",
      "format": "date-time",
      "description": "ISO 8601 timestamp of final failure",
      "example": "2025-03-01T12:10:01.234Z"
    }
  }
}
```

### Example Payload

```json
{
  "fileId": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
  "caseId": "c-2025-001234",
  "error": "CLAMD_UNAVAILABLE",
  "message": "Clamd remained unavailable after 3 retry attempts",
  "retryCount": 3,
  "retryHistory": [
    {
      "attempt": 1,
      "error": "CLAMD_UNAVAILABLE",
      "failedAt": "2025-03-01T12:00:01.234Z",
      "retryQueue": "q.file.scan.retry-1"
    },
    {
      "attempt": 2,
      "error": "CLAMD_UNAVAILABLE",
      "failedAt": "2025-03-01T12:02:01.234Z",
      "retryQueue": "q.file.scan.retry-2"
    },
    {
      "attempt": 3,
      "error": "CLAMD_UNAVAILABLE",
      "failedAt": "2025-03-01T12:10:01.234Z",
      "retryQueue": "q.file.scan.retry-3"
    }
  ],
  "originalMessage": {
    "fileId": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
    "caseId": "c-2025-001234",
    "tempPath": "/mnt/temp-nfs/uploads/f47ac10b-58cc-4372-a567-0e02b2c3d479",
    "originalName": "report_2025.pdf",
    "sizeBytes": 2457600,
    "contentType": "application/pdf",
    "uploadedAt": "2025-03-01T12:00:00Z"
  },
  "failedAt": "2025-03-01T12:10:01.234Z"
}
```

---

## 6. CaseCancelledMessage (Inbound)

**Purpose**: Signals cancellation of a case and all related file scans  
**Queue**: `q.case.cancelled`  
**Exchange**: `uploader.exchange`  
**Source**: Java Backend  
**Consumer**: ClamGo scanner

### JSON Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "title": "CaseCancelledMessage",
  "description": "Inbound message to cancel case processing and related file scans",
  "required": ["caseId", "cancelledBy", "cancelledAt", "fileIds"],
  "properties": {
    "caseId": {
      "type": "string",
      "description": "Case identifier to cancel",
      "example": "c-2025-001234"
    },
    "cancelledBy": {
      "type": "string",
      "description": "User ID or system identifier who cancelled",
      "example": "user-12345"
    },
    "cancelledAt": {
      "type": "string",
      "format": "date-time",
      "description": "ISO 8601 timestamp of cancellation",
      "example": "2025-03-01T12:05:00Z"
    },
    "fileIds": {
      "type": "array",
      "description": "List of file IDs associated with this case",
      "items": {
        "type": "string"
      },
      "example": [
        "f47ac10b-58cc-4372-a567-0e02b2c3d479",
        "f47ac10b-58cc-4372-a567-0e02b2c3d480"
      ]
    }
  }
}
```

### Example Payload

```json
{
  "caseId": "c-2025-001234",
  "cancelledBy": "user-12345",
  "cancelledAt": "2025-03-01T12:05:00Z",
  "fileIds": [
    "f47ac10b-58cc-4372-a567-0e02b2c3d479",
    "f47ac10b-58cc-4372-a567-0e02b2c3d480",
    "f47ac10b-58cc-4372-a567-0e02b2c3d481"
  ]
}
```

### Processing Notes

- **In-Memory Tracking**: Case ID is added to scanner's cancelled set
- **Redis Caching**: Also stored in Redis with key `cancelled:{caseId}` for fast lookups
- **Message Handling**: Any FileUploadedMessage for cancelled case is discarded (ACKed)
- **Pre & Post-Scan**: Cancellation checked before AND after file read

---

## RabbitMQ Configuration

### Exchanges

```yaml
uploader.exchange:
  type: direct
  durable: true
  auto_delete: false

uploader.dlx:
  type: direct
  durable: true
  auto_delete: false
```

### Queues

```yaml
q.file.scan:
  durable: true
  queue_arguments:
    x-dead-letter-exchange: uploader.dlx

q.file.scan.retry-1:
  durable: true
  queue_arguments:
    x-dead-letter-exchange: uploader.exchange
    x-dead-letter-routing-key: file.scan.retry.expired
    x-message-ttl: 30000

q.file.scan.retry-2:
  durable: true
  queue_arguments:
    x-dead-letter-exchange: uploader.exchange
    x-dead-letter-routing-key: file.scan.retry.expired
    x-message-ttl: 120000

q.file.scan.retry-3:
  durable: true
  queue_arguments:
    x-dead-letter-exchange: uploader.exchange
    x-dead-letter-routing-key: file.scan.retry.expired
    x-message-ttl: 600000

q.case.cancelled:
  durable: true

scan.dead.q:
  durable: true
```

### Bindings

```yaml
# Scan request queue
q.file.scan:
  exchange: uploader.exchange
  routing_key: file.upload.completed

# Case cancellation queue
q.case.cancelled:
  exchange: uploader.exchange
  routing_key: case.cancelled

# Results are fanout (all interested parties receive)
# Routing keys: file.scan.completed, file.scan.retrying
# Routing key: file.scan.failed -> uploader.dlx
```

---

## Error Codes

Common error codes included in messages:

| Error Code | Meaning | Recoverable |
|---|---|---|
| `CLAMD_UNAVAILABLE` | Cannot connect to ClamAV daemon | Yes (retry) |
| `FILE_NOT_FOUND` | Temp file not found | No (discard) |
| `SCAN_ERROR` | Error during clamd scan | Yes (retry) |
| `PATH_VALIDATION_FAILED` | File path outside allowed prefix | No (discard) |
| `FILE_READ_ERROR` | I/O error reading file | Yes (retry) |
| `MAGIC_BYTE_DETECTION_ERROR` | MIME type detection failed | No (continue) |

---

## Verdict Types

| Verdict | Meaning | Action |
|---|---|---|
| `CLEAN` | No malware detected | Continue processing |
| `INFECTED` | Malware signature matched | Flag for investigation |
| `ERROR` | Scan could not complete | Retry or manual review |

---

## Magic Byte Consistency Levels

| Consistency | Meaning | Risk Level |
|---|---|---|
| `CONSISTENT` | Detected MIME matches claimed | Low |
| `MINOR_MISMATCH` | Related types (e.g., PDF vs PDF-A) | Low |
| `MISMATCH` | Different type families | High |
| `UNKNOWN` | Detection failed but file valid | Medium |
| `EMPTY` | Empty file | Low |

---

## Usage Examples

### Publishing FileUploadedMessage (Go)

```go
import (
  "encoding/json"
  "time"
  rmq "github.com/kubenetic/BunnyShepherd/pkg/rabbitmq"
)

msg := model.FileUploadedMessage{
  FileId:       "f47ac10b-58cc-4372-a567-0e02b2c3d479",
  CaseId:       "c-2025-001234",
  TempPath:     "/mnt/temp-nfs/uploads/f47ac10b-58cc-4372-a567-0e02b2c3d479",
  OriginalName: "document.pdf",
  SizeBytes:    2457600,
  ContentType:  "application/pdf",
  UploadedAt:   time.Now(),
}

body, _ := json.Marshal(msg)
_ = publisher.Publish(
  ctx,
  exchange,
  "file.upload.completed",
  body,
  nil,
)
```

### Consuming ScanCompletedMessage (Go)

```go
var result model.ScanCompletedMessage
_ = json.Unmarshal(delivery.Body, &result)

if result.Verdict == model.VerdictInfected {
  log.Warn().Str("threatName", result.ThreatName).Msg("Malware detected!")
}
```

### RabbitMQ Management UI

Access via `http://localhost:15672` (default credentials: guest/guest)

- Monitor queue depths
- View message payloads
- Purge queues for testing
- Configure bindings

---

## Testing Messages

### CLI: Send Test Message

```bash
# Using amqp-cli or similar tool
echo '{
  "fileId": "test-123",
  "caseId": "test-case-1",
  "tempPath": "/mnt/temp-nfs/test.txt",
  "originalName": "test.txt",
  "sizeBytes": 1024,
  "contentType": "text/plain",
  "uploadedAt": "2025-03-01T12:00:00Z"
}' | rabbitmqctl export_message q.file.scan
```

### Docker: Interactive Testing

```bash
docker run -it --rm \
  --network host \
  -e RABBITMQ_HOST=localhost \
  nicolaka/netcat nc -l -p 5672
```

---

## References

- [Go Message Models](../pkg/model/)
- [Protocol Buffers Definition](../api/scan.proto)
- [RabbitMQ Configuration](../configs/config.yaml)
- [Main Scanner Logic](../pkg/scanner/scanner.go)

