// plexiptv turns Xtream Codes / M3U IPTV sources into HDHomeRun tuners for Plex.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/kardianos/service"

	"plexiptv/internal/app"
	"plexiptv/internal/config"
	"plexiptv/internal/logbuf"
	"plexiptv/internal/web"
)

var version = "dev"

type program struct {
	dataDir string
	cancel  context.CancelFunc
	done    chan struct{}
}

func (p *program) Start(s service.Service) error {
	cfg, err := config.Load(p.dataDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logs := logbuf.New(1000)
	log := newLogger(cfg, logs)
	log.Info("starting plexiptv", "version", version, "data", cfg.DataDir)

	a, err := app.New(cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		if err := a.Run(ctx, web.New(a, logs)); err != nil {
			log.Error("server stopped", "err", err)
			if service.Interactive() {
				os.Exit(1)
			}
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
	return nil
}

func newLogger(cfg config.Config, logs *logbuf.Buffer) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	var out io.Writer = os.Stderr
	logPath := filepath.Join(cfg.DataDir, "plexiptv.log")
	if st, err := os.Stat(logPath); err == nil && st.Size() > 10<<20 {
		os.Rename(logPath, logPath+".1")
	}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		out = io.MultiWriter(f, safeWriter{os.Stderr})
	}
	return slog.New(slog.NewTextHandler(logs.Tee(out), &slog.HandlerOptions{Level: level}))
}

// safeWriter swallows errors (stderr is invalid when running as a service).
type safeWriter struct{ w io.Writer }

func (s safeWriter) Write(p []byte) (int, error) {
	s.w.Write(p)
	return len(p), nil
}

func main() {
	dataDir := flag.String("data", config.DefaultDataDir(), "data directory (config, database, guides)")
	svcCmd := flag.String("service", "", "manage the system service: install | uninstall | start | stop | restart")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	abs, err := filepath.Abs(*dataDir)
	if err == nil {
		*dataDir = abs
	}

	prg := &program{dataDir: *dataDir}
	svc, err := service.New(prg, &service.Config{
		Name:        "plexiptv",
		DisplayName: "plexiptv IPTV Tuner",
		Description: "Emulates HDHomeRun tuners for Plex from IPTV (Xtream / M3U) sources.",
		Arguments:   []string{"-data", *dataDir},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *svcCmd != "" {
		if err := service.Control(svc, *svcCmd); err != nil {
			fmt.Fprintf(os.Stderr, "service %s failed: %v\n", *svcCmd, err)
			os.Exit(1)
		}
		fmt.Printf("service %s: ok\n", *svcCmd)
		return
	}
	if err := svc.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
