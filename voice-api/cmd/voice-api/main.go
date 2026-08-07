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

	"github.com/orchesta/callu-voice/internal/api"
	"github.com/orchesta/callu-voice/internal/call"
	"github.com/orchesta/callu-voice/internal/config"
	"github.com/orchesta/callu-voice/internal/notify"
	"github.com/orchesta/callu-voice/internal/telephony"
	"github.com/orchesta/callu-voice/internal/trunk"
	"github.com/orchesta/callu-voice/internal/tts"
)

const (
	// Slack on top of the callback ladder: the queued progress callbacks a terminal status waits
	// behind, plus scheduling.
	drainMargin = 6 * time.Second

	// A shutdown longer than this is worse than the callback it is waiting on. An operator who
	// configures a retry ladder past it is told at startup rather than left to discover that
	// drain gives up early.
	drainCeiling = 120 * time.Second
)

const (
	// What POST /calls still has to do after the render: one bounded ARI originate
	// (telephony's per-operation timeout) plus its surrounding bookkeeping.
	originateBudget = 15 * time.Second

	// Slack so the write deadline is never the thing that fires first.
	writeTimeoutMargin = 15 * time.Second
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// readiness answers /health: ready only when the ARI pump is alive and the synthesizer can serve.
type readiness struct {
	ari *telephony.ARI
	tts *tts.Client
}

func (r readiness) Ready(ctx context.Context) (bool, map[string]string) {
	detail := make(map[string]string, 3)
	ok := true

	if ariOK, msg := r.ari.Ready(ctx); ariOK {
		detail["ari"] = "ok"
	} else {
		detail["ari"] = msg
		ok = false
	}
	if r.tts.Healthy(ctx) {
		detail["tts"] = "ok"
	} else {
		detail["tts"] = "unavailable"
		ok = false
	}

	// Reported separately from ari, because it answers a different question: Asterisk can be
	// perfectly healthy while the carrier has stopped taking our calls.
	state, degraded := r.ari.TrunkState()
	detail["trunk"] = state
	if degraded {
		ok = false
	}
	return ok, detail
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ari, err := connect(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer ari.Close()

	ttsClient := tts.New(cfg.TTSURL, cfg.TTSTimeout)
	notifier := notify.New(cfg.CallbackTimeout, cfg.CallbackAttempts, cfg.CallbackBackoff, cfg.CallbackAllowPrivate, log)
	manager := call.NewManager(ari, ttsClient, notifier, cfg.MaxConcurrentCalls, log)

	// /health cannot tell a carrier the operator removed from one that has stopped answering, so
	// the thing that applies the change tells it.
	trunks := trunk.NewManager(trunk.ConfigPath)
	trunks.Watch(ari.NoteCarrier)

	// Derived from the retry ladder rather than fixed: a call aborted mid-page still has to
	// deliver its terminal status, and a drain shorter than the ladder throws that away — which
	// leaves the incident stuck at "connected", the one thing draining exists to prevent.
	drainGrace := notifier.Budget() + drainMargin
	if drainGrace > drainCeiling {
		log.Warn("callback retry ladder is longer than the shutdown budget; a terminal status may be lost on shutdown",
			"ladder", notifier.Budget(), "drain", drainCeiling)
		drainGrace = drainCeiling
	}
	log.Info("shutdown budget", "drain", drainGrace)

	pumpDone := make(chan struct{})
	go func() {
		ari.Run(ctx, manager.Dispatch)
		close(pumpDone)
	}()

	// Derived, not a constant: POST /calls renders synchronously (up to TTS_TIMEOUT)
	// before it dials, so a fixed deadline would cut the caller off mid-render.
	writeTimeout := cfg.TTSTimeout + originateBudget + writeTimeoutMargin

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewServer(manager, cfg, readiness{ari: ari, tts: ttsClient}, trunks, ttsClient, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	var fatal error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case <-pumpDone:
		fatal = errors.New("ari event pump exited; restarting")
		log.Error("ari pump down; draining and exiting")
	case err := <-serverErr:
		fatal = err
		log.Error("http server failed", "error", err)
	}

	// Drain in-flight calls first so their terminal callbacks are delivered,
	// then stop serving.
	drainCtx, cancel := context.WithTimeout(context.Background(), drainGrace)
	defer cancel()
	manager.Drain(drainCtx)

	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_ = server.Shutdown(shutdownCtx)

	return fatal
}

func connect(ctx context.Context, cfg config.Config, log *slog.Logger) (*telephony.ARI, error) {
	opts := telephony.Options{
		Application:    cfg.ARIApplication,
		URL:            cfg.ARIURL,
		WebsocketURL:   cfg.ARIWebsocket,
		Username:       cfg.ARIUsername,
		Password:       cfg.ARIPassword,
		EndpointFormat: cfg.EndpointFormat,
		TrunkEndpoint:  cfg.TrunkEndpoint,
		Context:        cfg.DialplanContext,
		Extension:      cfg.DialplanExtension,
		DetectMachine:  cfg.DetectMachine,
		CallerID:       cfg.CallerID,
		DialTimeout:    cfg.DialTimeout,
	}

	var lastErr error
	for attempt := 1; attempt <= 30; attempt++ {
		ari, err := telephony.Connect(ctx, opts, log)
		if err == nil {
			log.Info("connected to asterisk", "app", cfg.ARIApplication)
			return ari, nil
		}
		lastErr = err
		log.Warn("waiting for asterisk", "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}
