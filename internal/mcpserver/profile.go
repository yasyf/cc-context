package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
)

func serveProfiles(ctx context.Context) (func(), error) {
	dir, err := cache.Dir(ctx, "pprof")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, strconv.Itoa(os.Getpid())+".sock")
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale profile socket: %w", err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("listen on profile socket: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("restrict profile socket: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("mcpserver: profile socket", "err", err)
		}
	}()
	return func() {
		_ = srv.Close()
		_ = os.Remove(sock)
	}, nil
}
