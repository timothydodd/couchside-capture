// Command capture shows a web page as a live HLS stream, for a Couchside
// stream channel. See README.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/timothydodd/couchside-capture/internal/capture"
)

var version = "dev" // set by the release build

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	cfg, err := capture.FromEnv()
	if err != nil {
		slog.Error(err.Error())
		os.Exit(2)
	}
	if err := capture.TestHWAccel(cfg); err != nil {
		slog.Warn("the GPU encoder doesn't work here; encoding in software", "err", err)
		cfg.HWAccel = ""
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := capture.NewServer(cfg)
	srv := &http.Server{Addr: cfg.Addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen", "err", err)
			stop()
		}
	}()
	slog.Info("couchside-capture", "version", version, "addr", cfg.Addr, "showing", cfg.Describe())
	s.Run(ctx) // until a signal
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = srv.Shutdown(shutdown)
}
