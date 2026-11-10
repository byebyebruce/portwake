// Command portwake is an on-demand traffic entry for docker compose projects.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"portwake/internal/app"
	"portwake/internal/compose"
	"portwake/internal/events"
	"portwake/internal/tui"
)

func main() {
	var (
		dbPath   string
		webAddr  string
		importF  string
		configF  string
		useTUI   bool
		grace    time.Duration
		logLevel string
	)
	root := &cobra.Command{
		Use:          "portwake",
		Short:        "On-demand traffic entry for docker compose projects",
		Long:         "portwake listens on entry ports, starts the matching docker compose project on the first connection, forwards traffic once it is ready and stops it again when idle.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			docker, err := exec.LookPath("docker")
			if err != nil {
				return fmt.Errorf("docker not found on PATH: %w", err)
			}
			if configF != "" && importF != "" {
				return fmt.Errorf("--config and --import cannot be used together")
			}
			opts := app.Options{DBPath: dbPath, WebAddr: webAddr, Runner: compose.CLI{Docker: docker}, Grace: grace, ConfigPath: configF}
			if importF != "" {
				b, err := os.ReadFile(importF)
				if err != nil {
					return err
				}
				opts.Import = b
			}
			logger := log.NewWithOptions(os.Stderr, log.Options{ReportTimestamp: true, TimeFormat: "15:04:05"})
			if lvl, err := log.ParseLevel(logLevel); err == nil {
				logger.SetLevel(lvl)
			}
			return run(opts, useTUI, logger)
		},
	}
	f := root.Flags()
	f.StringVar(&dbPath, "db", "portwake.db", "SQLite database for events and stats (and configuration in db mode)")
	f.StringVar(&webAddr, "web", "127.0.0.1:9000", "web UI listen address")
	f.StringVar(&importF, "import", "", "YAML file replacing the stored configuration at start (db mode)")
	f.StringVar(&configF, "config", "", "YAML file used as the live configuration; web edits are written back to it")
	f.BoolVar(&useTUI, "tui", false, "show the terminal dashboard instead of logs")
	f.DurationVar(&grace, "grace", 10*time.Second, "how long to wait for connections on shutdown")
	f.StringVar(&logLevel, "log-level", "info", "log level when not using --tui (debug, info, warn, error)")
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(opts app.Options, useTUI bool, logger *log.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(opts)
	if err != nil {
		return err
	}
	if !useTUI {
		ch, cancel := a.Bus().Subscribe(4096)
		defer cancel()
		go logEvents(logger, ch)
	}
	if err := a.Start(ctx); err != nil {
		return err
	}
	if useTUI {
		err = tui.Run(ctx, a, a.WebAddr())
	} else {
		logger.Info("portwake started", "web", "http://"+a.WebAddr(), "projects", len(a.Projects()))
		<-ctx.Done()
		logger.Info("shutting down")
	}
	stop()
	if serr := a.Shutdown(context.Background()); err == nil {
		err = serr
	}
	return err
}

func logEvents(logger *log.Logger, ch <-chan events.Event) {
	for e := range ch {
		lvl, text := events.Describe(e)
		l := logger
		if e.Project != "" {
			l = logger.WithPrefix(e.Project)
		}
		switch lvl {
		case events.LevelDebug:
			l.Debug(text)
		case events.LevelInfo:
			l.Info(text)
		case events.LevelWarn:
			l.Warn(text)
		default:
			l.Error(text)
		}
	}
}
