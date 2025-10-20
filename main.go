package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/kijimaD/xruler/internal/cli"
)

func main() {
	// ログレベルの設定（環境変数 LOG_LEVEL で制御）
	logLevel := slog.LevelInfo
	if level := os.Getenv("LOG_LEVEL"); level != "" {
		switch strings.ToLower(level) {
		case "debug":
			logLevel = slog.LevelDebug
		case "info":
			logLevel = slog.LevelInfo
		case "warn":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		}
	}

	// slogの初期化
	opts := &slog.HandlerOptions{
		Level: logLevel,
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, opts))
	slog.SetDefault(logger)

	slog.Info("xruler starting", "log_level", logLevel.String())

	cmd := cli.NewCommand()

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		slog.Error("application error", "error", err)
		os.Exit(1)
	}
}
