package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ClamGo/pkg/scanner"

	rmq "github.com/kubenetic/BunnyShepherd/pkg/rabbitmq"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"
	"github.com/spf13/viper"
)

// Version and BuildTime are set at build time via ldflags.
var (
	Version   = "dev"
	BuildTime = "unknown"
)

func init() {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("./configs/")
	viper.AddConfigPath("/etc/clamgo/")
	viper.SetEnvPrefix("CLAMGO")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AllowEmptyEnv(true)
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		log.Warn().Err(err).Msg("no config file found, using defaults and environment")
	}

	// Clamd connection
	viper.SetDefault("clamd.tcp.addr", "localhost:3310")
	viper.SetDefault("clamd.unix.path", "")

	// RabbitMQ
	viper.SetDefault("rabbitmq.host", "127.0.0.1")
	viper.SetDefault("rabbitmq.port", 5672)
	viper.SetDefault("rabbitmq.username", "")
	viper.SetDefault("rabbitmq.password", "")
	viper.SetDefault("rabbitmq.vhost", "/")
	viper.SetDefault("rabbitmq.exchange", "uploader.exchange")
	viper.SetDefault("rabbitmq.dlx", "uploader.dlx")
	viper.SetDefault("rabbitmq.scanQueue", "q.file.scan")
	viper.SetDefault("rabbitmq.cancelQueue", "q.case.cancelled")
	viper.SetDefault("rabbitmq.scanCompletedRoutingKey", "file.scan.completed")
	viper.SetDefault("rabbitmq.scanRetryingRoutingKey", "file.scan.retrying")
	viper.SetDefault("rabbitmq.dlqRoutingKey", "file.scan.failed")
	viper.SetDefault("rabbitmq.scanStartedRoutingKey", "file.scan.started")
	viper.SetDefault("rabbitmq.prefetchCount", 1)
	viper.SetDefault("rabbitmq.scanHandlerTimeoutSeconds", 600)
	viper.SetDefault("rabbitmq.cancelPrefetchCount", 10)

	// Redis
	viper.SetDefault("redis.enabled", false)
	viper.SetDefault("redis.addr", "127.0.0.1:6379")
	viper.SetDefault("redis.password", "")
	viper.SetDefault("redis.db", 0)

	// Redis Cluster
	viper.SetDefault("redis.cluster.enabled", false)
	viper.SetDefault("redis.cluster.nodes", []string{"redis-0:6379", "redis-1:6379", "redis-2:6379"})
	viper.SetDefault("redis.cluster.password", "")
	viper.SetDefault("redis.cluster.maxRedirects", 10)

	// Temp NFS
	viper.SetDefault("tempNFS.prefix", "/mnt/temp-nfs/")

	// Scanner
	viper.SetDefault("scanner.maxFileSizeBytes", 500*1024*1024) // 500 MB
	viper.SetDefault("scanner.staleFilesLogDir", "/var/lib/clamgo")

	// Health server
	viper.SetDefault("health.port", 8080)

	// Logging
	viper.SetDefault("logging.level", "info")

	log.Logger = zerolog.New(os.Stdout).
		With().
		Timestamp().
		Caller().
		Logger()
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
	switch viper.GetString("logging.level") {
	case "trace":
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case "fatal":
		zerolog.SetGlobalLevel(zerolog.FatalLevel)
	case "panic":
		zerolog.SetGlobalLevel(zerolog.PanicLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}

func main() {
	// SIGTERM / SIGINT context — cancellation stops consumers.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancel()

	// Initialize RabbitMQ connection manager.
	// Password is read through Viper (CLAMGO_RABBITMQ_PASSWORD env var) — consistent
	// with all other config. Credentials are passed via SASL PlainAuth, not embedded in the URI.
	rmqPassword := viper.GetString("rabbitmq.password")
	if rmqPassword == "" {
		log.Fatal().Msg("rabbitmq.password (CLAMGO_RABBITMQ_PASSWORD) is required but not set")
	}

	rmqHost := viper.GetString("rabbitmq.host")
	rmqPort := viper.GetInt("rabbitmq.port")
	rmqUsername := viper.GetString("rabbitmq.username")
	rmqVhost := viper.GetString("rabbitmq.vhost")
	if rmqVhost == "" {
		rmqVhost = "/"
	}

	// Build URI without credentials (host:port only).
	amqpURI := fmt.Sprintf("amqp://%s:%d", rmqHost, rmqPort)

	// Configure AMQP with SASL plain auth (credentials not in URI).
	amqpConfig := amqp.Config{
		SASL: []amqp.Authentication{
			&amqp.PlainAuth{
				Username: rmqUsername,
				Password: rmqPassword,
			},
		},
		Vhost: rmqVhost,
	}

	mqConn, err := rmq.NewConnectionManager(ctx, amqpURI, &amqpConfig)
	if err != nil {
		log.Fatal().Err(err).Msgf("failed to connect to RabbitMQ at %s/%s", rmqHost, rmqVhost)
	}
	defer mqConn.Close()
	log.Info().Msgf("connected to RabbitMQ at %s:%d/%s", rmqHost, rmqPort, rmqVhost)

	// Initialize publisher (with confirms).
	pub, err := rmq.NewPublisher(mqConn)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create RabbitMQ publisher")
	}
	defer pub.Close()

	// Initialize Redis client (optional: used for cancelled:{caseId} fast-check).
	var redisClient redis.UniversalClient

	if viper.GetBool("redis.enabled") {
		clusterEnabled := viper.GetBool("redis.cluster.enabled")

		if clusterEnabled {
			// Cluster mode
			nodes := viper.GetStringSlice("redis.cluster.nodes")
			if len(nodes) > 0 {
				redisClient = redis.NewClusterClient(&redis.ClusterOptions{
					Addrs:        nodes,
					Password:     viper.GetString("redis.cluster.password"),
					MaxRedirects: viper.GetInt("redis.cluster.maxRedirects"),
				})

				log.Info().
					Strs("nodes", nodes).
					Msg("Redis Cluster mode enabled")
			}
		} else {
			// Single node mode (backward compatibility)
			redisAddr := viper.GetString("redis.addr")
			if redisAddr != "" {
				redisClient = redis.NewClient(&redis.Options{
					Addr:     redisAddr,
					Password: viper.GetString("redis.password"),
					DB:       viper.GetInt("redis.db"),
				})

				log.Info().
					Str("addr", redisAddr).
					Msg("Redis single-node mode enabled")
			}
		}

		// Ping test (same for both modes)
		if redisClient != nil {
			pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
			pingErr := redisClient.Ping(pingCtx).Err()
			pingCancel()
			if pingErr != nil {
				log.Warn().Err(pingErr).Msg("Redis connection failed; cancellation Redis check will be disabled")
				redisClient = nil
			}

			if redisClient != nil {
				defer redisClient.Close()
				log.Info().Msg("Redis connected successfully")
			}
		}
	} else {
		log.Info().Msg("Redis disabled; using in-memory cancellation tracking only")
	}

	// Build scanner.
	scannerCfg := scanner.Config{
		TempNFSPrefix:           viper.GetString("tempNFS.prefix"),
		Exchange:                viper.GetString("rabbitmq.exchange"),
		DLX:                     viper.GetString("rabbitmq.dlx"),
		ScanCompletedRoutingKey: viper.GetString("rabbitmq.scanCompletedRoutingKey"),
		ScanRetryingRoutingKey:  viper.GetString("rabbitmq.scanRetryingRoutingKey"),
		DLQRoutingKey:           viper.GetString("rabbitmq.dlqRoutingKey"),
		ScanStartedRoutingKey:   viper.GetString("rabbitmq.scanStartedRoutingKey"),
		ClamdTCPAddr:            viper.GetString("clamd.tcp.addr"),
		ClamdUnixPath:           viper.GetString("clamd.unix.path"),
		MaxFileSizeBytes:        viper.GetInt64("scanner.maxFileSizeBytes"),
		StaleFilesLogDir:        viper.GetString("scanner.staleFilesLogDir"),
	}

	// Validate TempNFSPrefix
	if scannerCfg.TempNFSPrefix == "" {
		log.Fatal().Msg("tempNFS.prefix must be configured and non-empty")
	}
	if !strings.HasSuffix(scannerCfg.TempNFSPrefix, string(filepath.Separator)) {
		scannerCfg.TempNFSPrefix += string(filepath.Separator)
	}
	if !filepath.IsAbs(scannerCfg.TempNFSPrefix) {
		log.Fatal().Str("prefix", scannerCfg.TempNFSPrefix).Msg("tempNFS.prefix must be an absolute path")
	}

	s := scanner.New(scannerCfg, pub, redisClient)
	s.StartCleanup(ctx)

	// Build consumers (prefetch=1 for both: process one message at a time).
	prefetch := viper.GetInt("rabbitmq.prefetchCount")
	scanHandlerTimeout := time.Duration(viper.GetInt("rabbitmq.scanHandlerTimeoutSeconds")) * time.Second

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(prefetch), rmq.WithMessageHandlerTimeout(scanHandlerTimeout))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create scan consumer")
	}
	defer scanConsumer.Close()

	cancelPrefetch := viper.GetInt("rabbitmq.cancelPrefetchCount")
	cancelConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(cancelPrefetch))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create cancel consumer")
	}
	defer cancelConsumer.Close()

	scanQueue := viper.GetString("rabbitmq.scanQueue")
	cancelQueue := viper.GetString("rabbitmq.cancelQueue")

	// Start health check HTTP server.
	// Binds to all interfaces so that Istio's pilot-agent can forward
	// liveness/readiness probes from the pod IP to this endpoint.
	healthPort := viper.GetInt("health.port")
	healthAddr := fmt.Sprintf(":%d", healthPort)

	// Bind synchronously to detect port-in-use errors at startup.
	healthListener, err := net.Listen("tcp", healthAddr)
	if err != nil {
		log.Fatal().Err(err).Str("addr", healthAddr).Msg("failed to bind health server port")
	}

	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status":    "ok",
			"version":   Version,
			"buildTime": BuildTime,
		}); err != nil {
			log.Error().Err(err).Msg("health encode failed")
			http.Error(w, "internal", http.StatusInternalServerError)
		}
	})
	healthSrv := &http.Server{
		Handler: healthMux,
	}

	// Serve on the bound listener in a goroutine.
	go func() {
		log.Info().Str("addr", healthAddr).Msg("health check server started")
		if err := healthSrv.Serve(healthListener); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("health check server exited unexpectedly")
		}
	}()

	// Start case.cancelled consumer in background.
	go func() {
		tag := rmq.GenConsumerTag("cancel")
		log.Info().Str("queue", cancelQueue).Msg("starting case-cancelled consumer")
		if err := cancelConsumer.Subscribe(ctx, cancelQueue, tag, s.HandleCancelMessage); err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("case-cancelled consumer stopped unexpectedly")
		}
	}()

	// Start scan consumer — blocks until context is cancelled (SIGTERM).
	log.Info().Str("queue", scanQueue).Msg("starting scan consumer")
	tag := rmq.GenConsumerTag("scan")
	if err := scanConsumer.Subscribe(ctx, scanQueue, tag, s.HandleScanMessage); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("scan consumer stopped unexpectedly")
		os.Exit(1)
	}

	// Gracefully shut down the health server now that consumers have stopped.
	// This prevents Kubernetes from routing probe traffic to a pod that is no
	// longer processing messages.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if err := healthSrv.Shutdown(shutCtx); err != nil {
		log.Warn().Err(err).Msg("health check server shutdown error")
	}

	log.Info().Msg("ClamGo shutdown complete")
}
