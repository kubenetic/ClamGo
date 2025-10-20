flowchart LR
subgraph Client["Browser (UI)"]
U1[Submit form\n-> 202 + job_id]
U2[Upload via tusd\n(job_id)]
U3[SSE /jobs/{jobId}/events]
end

subgraph Spring["Spring Boot (MVC)"]
B1[Create job\nstore metadata]
B2[Track uploads\nfrom tusd hooks]
B3[Capacity gate & per-job window\n(enqueue per-file)]
B4[Consume results\npersist & SSE broadcast]
B5[SSE Registry\n(jobId -> emitters)]
B6[DB/Cache\n(job snapshot + report)]
end

subgraph TUSD["tusd server"]
T1[pre-create hook\nauth via session/backend]
T2[write to temp storage]
T3[post-finish hook\nnotify backend (file done)]
end

subgraph MQ["RabbitMQ"]
XJ[Exchange: scan.jobs (direct)]
QJ((Queue: scan.jobs.q))
XR[Exchange: scan.results (fanout)]
QRi((Per-pod queue: backend.scan.results.$POD_NAME))
XD[scan.dead.x]:::dlx
QD((scan.dead.q)):::dlq
end

subgraph Scanner["Go wrapper + ClamAV"]
W1[Worker pool\n(15 total, 1 clamd conn/worker)]
C1[clamd]
end

classDef dlx fill:#fee,stroke:#e66;
classDef dlq fill:#fee,stroke:#e66;

U1 -->|HTTP POST| B1 -->|202 + job_id| U1
U2 --> T1 -->|auth check| B1
T1 --> T2 --> T3 -->|file-finished| B2
B3 -->|Publish per-file| XJ --> QJ
W1 -->|Consume| QJ
W1 -->|SCAN| C1
W1 -->|fileStarted/fileResult| XR --> QRi
B4 -->|Consume| QRi
B4 -->|Update snapshot| B6
B5 -->|SSE events| U3

XD --> QD
