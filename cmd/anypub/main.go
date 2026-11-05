// Command anypub publishes Anytype objects tagged "publish" as static pages.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sker65/anypub/internal/config"
	"github.com/sker65/anypub/internal/service"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("config", os.Getenv("ANYPUB_CONFIG"), "path to anypub.yaml")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "anypub:", err)
		os.Exit(2)
	}
	log := newLogger(cfg.Obs.LogLevel).With("version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := service.Run(ctx, cfg, log); err != nil {
		log.Error("anypub stopped", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
