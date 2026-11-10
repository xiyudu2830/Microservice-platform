package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/xiyudu2830/Microservice-platform/internal/runtime"
)

func main() {
	if len(os.Args) != 2 {
		slog.Error("usage: governance {broker|auth|registry|audit|notify|echo}")
		os.Exit(2)
	}
	role := os.Args[1]
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", role))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runtime.Run(ctx, role); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}
