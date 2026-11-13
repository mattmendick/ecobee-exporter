// Copyright 2020 Billy Wooten
// Copyright 2026 ecobee-exporter contributors
// Licensed under the Apache License, Version 2.0. See LICENSE.
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

	"github.com/mattmendick/ecobee-exporter/collector"
	"github.com/mattmendick/ecobee-exporter/internal/ecobee"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var Version = "dev"
var GitCommit = "unknown"
var BuildStamp = "unknown"

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	if err := run(); err != nil {
		slog.Error("exporter stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("listen-address", env("ECOBEE_LISTEN_ADDRESS", ":9098"), "HTTP listen address")
	appKey := flag.String("appkey", env("ECOBEE_APPKEY", ""), "Existing ecobee application key (prefer ECOBEE_APPKEY)")
	cache := flag.String("cachefile", env("ECOBEE_CACHEFILE", "/db/auth.cache"), "Persistent token cache")
	intervalFlag := flag.String("poll-interval", env("ECOBEE_POLL_INTERVAL", "3m"), "API polling interval, at least 3m")
	maxAgeFlag := flag.String("max-cache-age", env("ECOBEE_MAX_CACHE_AGE", "15m"), "Omit telemetry after this long without a successful API poll")
	timeoutFlag := flag.String("api-timeout", env("ECOBEE_API_TIMEOUT", "30s"), "Timeout per API HTTP request")
	auth := flag.Bool("auth", false, "Authorize interactively, persist tokens, and exit")
	version := flag.Bool("version", false, "Print build information and exit")
	flag.Parse()
	if *version {
		fmt.Printf("ecobee-exporter %s (commit %s, built %s)\n", Version, GitCommit, BuildStamp)
		return nil
	}
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *appKey == "" {
		return errors.New("set ECOBEE_APPKEY or --appkey; there is no shared default key")
	}
	interval, err := time.ParseDuration(*intervalFlag)
	if err != nil || interval < 3*time.Minute {
		return errors.New("poll-interval must be at least 3m")
	}
	maxAge, err := time.ParseDuration(*maxAgeFlag)
	if err != nil || maxAge < interval {
		return errors.New("max-cache-age must be at least poll-interval")
	}
	timeout, err := time.ParseDuration(*timeoutFlag)
	if err != nil || timeout <= 0 || timeout > interval {
		return errors.New("api-timeout must be positive and no greater than poll-interval")
	}
	client, err := ecobee.NewClient(*appKey, *cache, timeout)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *auth {
		return client.Authorize(ctx, os.Stdin, os.Stdout)
	}
	c := collector.New(client, interval, maxAge)
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}), c)
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "ecobee_exporter_build_info", Help: "Exporter build information.", ConstLabels: prometheus.Labels{"version": Version, "revision": GitCommit}}, func() float64 { return 1 }))
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	server := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	pollCtx, cancelPoll := context.WithCancel(ctx)
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); c.Run(pollCtx) }()
	defer func() { cancelPoll(); <-pollDone }()
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()
	slog.Info("serving cached metrics", "address", *addr, "poll_interval", interval)
	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
