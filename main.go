package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
	viper.SetDefault("rabbitmq.exchange", "uploader.exchange")
	viper.SetDefault("rabbitmq.dlx", "uploader.dlx")
	viper.SetDefault("rabbitmq.scanQueue", "q.file.scan")
	viper.SetDefault("rabbitmq.cancelQueue", "q.case.cancelled")
	viper.SetDefault("rabbitmq.scanCompletedRoutingKey", "file.scan.completed")
	viper.SetDefault("rabbitmq.scanRetryingRoutingKey", "file.scan.retrying")
	viper.SetDefault("rabbitmq.dlqRoutingKey", "file.scan.failed")
	viper.SetDefault("rabbitmq.prefetchCount", 1)

	// Redis
	viper.SetDefault("redis.addr", "127.0.0.1:6379")
	viper.SetDefault("redis.password", "")
	viper.SetDefault("redis.db", 0)

	// Temp NFS
	viper.SetDefault("tempNFS.prefix", "/mnt/temp-nfs/")

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
	amqpURI := fmt.Sprintf("amqp://%s:%s@%s:%d/",
		viper.GetString("rabbitmq.username"),
		viper.GetString("rabbitmq.password"),
		viper.GetString("rabbitmq.host"),
		viper.GetInt("rabbitmq.port"),
	)
	mqConn, err := rmq.NewConnectionManager(ctx, amqpURI, &amqp.Config{})
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to RabbitMQ")
	}
	defer mqConn.Close()

	// Initialize publisher (with confirms).
	pub, err := rmq.NewPublisher(mqConn)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create RabbitMQ publisher")
	}
	defer pub.Close()

	// Initialize Redis client (optional: used for cancelled:{caseId} fast-check).
	var redisClient redis.UniversalClient
	redisAddr := viper.GetString("redis.addr")
	if redisAddr != "" {
		redisClient = redis.NewClient(&redis.Options{
			Addr:     redisAddr,
			Password: viper.GetString("redis.password"),
			DB:       viper.GetInt("redis.db"),
		})

		pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
		if err := redisClient.Ping(pingCtx).Err(); err != nil {
			log.Warn().Err(err).Msg("Redis ping failed; cancellation Redis check will be disabled")
			redisClient = nil
		}
		pingCancel()

		if redisClient != nil {
			defer redisClient.Close()
			log.Info().Str("addr", redisAddr).Msg("Redis connected")
		}
	}

	// Build scanner.
	scannerCfg := scanner.Config{
		TempNFSPrefix:           viper.GetString("tempNFS.prefix"),
		Exchange:                viper.GetString("rabbitmq.exchange"),
		DLX:                     viper.GetString("rabbitmq.dlx"),
		ScanCompletedRoutingKey: viper.GetString("rabbitmq.scanCompletedRoutingKey"),
		ScanRetryingRoutingKey:  viper.GetString("rabbitmq.scanRetryingRoutingKey"),
		DLQRoutingKey:           viper.GetString("rabbitmq.dlqRoutingKey"),
		ClamdTCPAddr:            viper.GetString("clamd.tcp.addr"),
		ClamdUnixPath:           viper.GetString("clamd.unix.path"),
	}
	s := scanner.New(scannerCfg, pub, redisClient)

	// Build consumers (prefetch=1 for both: process one message at a time).
	prefetch := viper.GetInt("rabbitmq.prefetchCount")

	scanConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(prefetch))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create scan consumer")
	}
	defer scanConsumer.Close()

	cancelConsumer, err := rmq.NewConsumer(mqConn, rmq.WithPrefetchCount(10))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create cancel consumer")
	}
	defer cancelConsumer.Close()

	scanQueue := viper.GetString("rabbitmq.scanQueue")
	cancelQueue := viper.GetString("rabbitmq.cancelQueue")

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

	log.Info().Msg("ClamGo shutdown complete")
}
