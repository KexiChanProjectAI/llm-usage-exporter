package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/collector"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/provider"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/source"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	logJSON := flag.Bool("log-json", false, "log as JSON")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid -log-level: %v\n", err)
		os.Exit(2)
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	log := slog.New(handler)

	if err := run(*configPath, log); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	provider.UserAgent = "llm-usage-exporter/" + version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(cfg.Database.DSN)
	if err != nil {
		return fmt.Errorf("parse database.dsn: %w", err) // pgx errors do not echo the password
	}
	poolCfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	exp := collector.New(cfg, func(ctx context.Context) ([]source.Account, error) {
		return source.LoadAccounts(ctx, pool)
	}, nil, log)

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	mux := http.NewServeMux()
	mux.Handle(cfg.MetricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !exp.Healthy() {
			http.Error(w, "sub2api database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	done := make(chan struct{})
	go func() {
		defer close(done)
		exp.Run(ctx)
	}()

	srvErr := make(chan error, 1)
	go func() {
		log.Info("serving metrics", "listen", cfg.Listen, "path", cfg.MetricsPath, "version", version,
			"providers", cfg.EnabledProviders())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err = <-srvErr:
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-done
	return err
}
