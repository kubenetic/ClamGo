package main

import (
    "context"
    "crypto/tls"
    "errors"
    "fmt"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "ClamGo/internal/http/handlers"
    "ClamGo/pkg/service/clamd"

    "github.com/fsnotify/fsnotify"
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
    "github.com/rs/zerolog/pkgerrors"
    "github.com/spf13/viper"
)

var ClamClient *clamd.ClamClient

func main() {
    viper.SetConfigName("config")
    viper.SetConfigType("yaml")
    viper.AddConfigPath("/etc/clamgo/")
    viper.AddConfigPath("$HOME/.clamgo/")
    viper.AddConfigPath("./configs/")
    viper.SetEnvPrefix("CLAMG_")
    viper.AllowEmptyEnv(true)
    viper.AutomaticEnv()

    if err := viper.ReadInConfig(); err != nil {
        log.Fatal().Err(err).Msg("error during reading config file")
    }

    viper.OnConfigChange(func(e fsnotify.Event) {
        log.Debug().Msg("config file changed")
    })
    viper.WatchConfig()

    viper.SetDefault("clamd.unix.readTimeout", 5*time.Second)
    viper.SetDefault("clamd.unix.writeTimeout", 5*time.Second)
    viper.SetDefault("rabbitmq.enabled", false)
    viper.SetDefault("application.logging.level", "info")
    viper.SetDefault("application.workerPool.size", 3)
    viper.SetDefault("application.workerPool.queueSize", 100)
    viper.SetDefault("application.server.host", "0.0.0.0")
    viper.SetDefault("application.server.port", 5050)
    viper.SetDefault("application.server.readTimeout", 10*time.Second)
    viper.SetDefault("application.server.writeTimeout", 10*time.Second)
    viper.SetDefault("application.server.shutdownTimeout", 60*time.Second)
    viper.SetDefault("application.server.tls.enabled", false)
    viper.SetDefault("application.server.tls.skipVerify", false)

    log.Logger = zerolog.New(os.Stdout).
        With().
        Timestamp().
        Caller().
        Logger()
    zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
    switch viper.GetString("application.logging.level") {
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

    ClamClient = clamd.NewClient()
    defer ClamClient.Close()

    mux := http.NewServeMux()
    clamdHandler := handlers.NewClamdHandler(ClamClient)

    mux.Handle("/ping", clamdHandler.Ping())
    mux.Handle("/version", clamdHandler.Version())
    mux.Handle("/stats", clamdHandler.Stats())
    mux.Handle("/scan", clamdHandler.Scan(context.TODO()))

    addr := fmt.Sprintf("%s:%d",
        viper.GetString("application.server.host"),
        viper.GetInt("application.server.port"))
    server := http.Server{
        Addr:    addr,
        Handler: mux,
        TLSConfig: &tls.Config{
            InsecureSkipVerify: viper.GetBool("application.server.tls.skipVerify"),
        },
        ReadTimeout:  viper.GetDuration("application.server.readTimeout"),
        WriteTimeout: viper.GetDuration("application.server.writeTimeout"),
        HTTP2:        nil,
    }

    termCtx, termCncl := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
    defer termCncl()

    go func() {
        tlsEnabled := viper.GetBool("application.server.tls.enabled")
        if tlsEnabled {
            crtFile := viper.GetString("application.server.tls.cert")
            keyFile := viper.GetString("application.server.tls.key")
            if err := server.ListenAndServeTLS(crtFile, keyFile); err != nil {
                if !errors.Is(err, http.ErrServerClosed) {
                    log.Fatal().Err(err).Msg("error starting server")
                }

                log.Info().Msg("server stopped successfully")
            }
        } else {
            if err := server.ListenAndServe(); err != nil {
                if !errors.Is(err, http.ErrServerClosed) {
                    log.Fatal().Err(err).Msg("error starting server")
                }

                log.Info().Msg("server stopped successfully")
            }
        }
    }()

    <-termCtx.Done()

    if err := server.Shutdown(context.Background()); err != nil {
        log.Fatal().Err(err).Msg("error shutting down server")
    }

    os.Exit(0)
}
