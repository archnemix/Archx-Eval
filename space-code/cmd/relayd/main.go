// Command relayd is the Archx-Eval relay: the API layer that sits between a
// user's front-end runner and the private evaluation bank.
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
	"strings"
	"syscall"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/bank"
	"github.com/archnemix/archx-eval/relay/internal/config"
	"github.com/archnemix/archx-eval/relay/internal/core"
	"github.com/archnemix/archx-eval/relay/internal/hf"
	"github.com/archnemix/archx-eval/relay/internal/httpapi"
	"github.com/archnemix/archx-eval/relay/internal/ratelog"
	"github.com/archnemix/archx-eval/relay/internal/reputation"
	"github.com/archnemix/archx-eval/relay/internal/store"
	"github.com/archnemix/archx-eval/relay/internal/syncctl"
)

func main() {
	var (
		healthcheck = flag.Bool("healthcheck", false, "probe the local /health endpoint and exit")
		printCfg    = flag.Bool("print-config", false, "print the resolved configuration and exit")
		sync        = flag.Bool("sync", false, "publish the local data tree to the private dataset and exit")
		dryRun      = flag.Bool("dry-run", false, "with -sync, write to SYNC_OUT_DIR instead of the dataset")
	)
	flag.Parse()

	if err := run(*healthcheck, *printCfg, *sync, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "relayd:", err)
		os.Exit(1)
	}
}

func run(healthcheck, printCfg, doSync, dryRun bool) error {
	cfg := config.Load()

	if printCfg {
		fmt.Printf("addr=%s\nbank_dir=%s\ndata_dir=%s\ndraw_max=%d\nwindow=%s\nstale_after=%d\nhf_enabled=%v\nsalt_default=%v\n",
			cfg.Addr, cfg.BankDir, cfg.DataDir, cfg.DrawMax, cfg.Window, cfg.StaleAfterUses, cfg.HFEnabled, cfg.SaltIsDefault())
		return nil
	}
	if healthcheck {
		return probeHealth(cfg)
	}

	log := newLogger(cfg.LogFormat)
	if cfg.SaltIsDefault() {
		log.Warn("RELAY_SALT is unset: using the insecure development salt. Every marker hash will change once a real salt is configured.")
	}
	if !cfg.HFEnabled {
		log.Warn("HF_DATA_TOKEN is unset: the dataset is neither read nor written")
	}

	// Pull our own state back from the dataset before opening the store, so the
	// process resumes with its memory of known callers intact.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 90*time.Second)
	if err := restoreState(bootCtx, cfg, log); err != nil {
		log.Error("restore failed; continuing with local state", "err", err)
	}
	// The bank is the answer key: it lives in the private dataset and nowhere
	// else. Peek at the local directory first so an operator-provided volume
	// wins over the dataset copy, then fetch only if it is empty.
	if !localBankPresent(cfg.BankDir) {
		if err := ensureBankDir(cfg.BankDir); err != nil {
			log.Error("bank directory unusable", "err", err)
		} else if err := restoreBank(bootCtx, cfg, log); err != nil {
			log.Error("bank restore failed; /health will report not-ready", "err", err)
		}
	} else {
		log.Info("local bank present; dataset bank not fetched", "dir", cfg.BankDir)
	}
	bootCancel()

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	bm, err := bank.Load(cfg.BankDir, cfg.StaleAfterUses)
	if err != nil {
		return err
	}
	rep, err := reputation.Load(cfg.ReputationFile)
	if err != nil {
		return err
	}
	if bm.Size() == 0 {
		log.Warn("the bank is empty: /health will report not-ready until examples are published")
	} else {
		log.Info("bank loaded", "examples", bm.Size(), "categories", len(bm.Categories()))
	}
	if rep.Loaded() {
		log.Info("reputation table loaded", "ranges", rep.Size())
	}

	// The rate log retention must exceed the longest window we evaluate, with
	// headroom, or a restart would silently forget evidence of a flood.
	retention := cfg.BanWindow
	if cfg.RateWindow > retention {
		retention = cfg.RateWindow
	}
	if cfg.GlobalWindow > retention {
		retention = cfg.GlobalWindow
	}
	retention *= 2
	rates, err := ratelog.Open(cfg.RateDir, retention)
	if err != nil {
		return err
	}
	defer rates.Close()

	svc := core.New(cfg, st, bm, rep, rates)
	log.Info("rate log ready", "keys", rates.Keys(), "retention", retention.String())

	// The dataset is a git repo: writes are sequential, so every state change
	// goes through one batching controller instead of being committed per
	// request. Enqueue never blocks a request.
	var controller *syncctl.Controller
	if cfg.HFEnabled {
		remote := newRemote(cfg)
		controller = syncctl.New(syncctl.Config{
			Interval:  cfg.SyncInterval,
			MaxFiles:  cfg.SyncMaxFiles,
			MaxQueued: 20000,
		}, remote, log)
		svc.WithSync(controller)
		syncCtx, syncCancel := context.WithCancel(context.Background())
		defer syncCancel()
		go controller.Run(syncCtx)
		defer controller.Stop()
		log.Info("dataset sync enabled", "interval", cfg.SyncInterval.String())
	} else {
		log.Warn("HF_DATA_TOKEN is unset: state stays local and is never published")
	}
	var _ hf.Remote

	if doSync {
		return syncToDataset(cfg, svc, log, dryRun)
	}

	srv := httpapi.New(cfg, svc, log, os.Getenv("ADMIN_TOKEN"))
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Housekeeping: expire raw identity rows and persist counters.
	go housekeep(ctx, svc, cfg, log)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "version", core.Version)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		log.Error("server error", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err := svc.Flush(); err != nil {
		log.Error("flush failed", "err", err)
	}
	if err := svc.FlushRates(); err != nil {
		log.Error("rate flush failed", "err", err)
	}
	return nil
}

func housekeep(ctx context.Context, svc *core.Service, cfg config.Config, log *slog.Logger) {
	// Bans still expire, so the pruner stays; there is no personal data left to
	// delete, which is the point.
	pruneEvery := 6 * time.Hour
	pruneTicker := time.NewTicker(pruneEvery)
	flushTicker := time.NewTicker(cfg.FlushInterval)
	rateTicker := time.NewTicker(10 * time.Minute)
	defer pruneTicker.Stop()
	defer flushTicker.Stop()
	defer rateTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pruneTicker.C:
			n, err := svc.Prune()
			if err != nil {
				log.Error("prune failed", "err", err)
				continue
			}
			if n > 0 {
				log.Info("cleared expired bans", "count", n)
			}
			ledgerFiles, subnetBans, err := svc.PruneLedger(time.Now().UTC())
			if err != nil {
				log.Error("ledger prune failed", "err", err)
			} else if ledgerFiles > 0 || subnetBans > 0 {
				log.Info("pruned stored bans and ledger days",
					"ledger_files", ledgerFiles, "subnet_bans", subnetBans)
			}
		case <-flushTicker.C:
			if err := svc.Flush(); err != nil {
				log.Error("flush failed", "err", err)
			}
		case <-rateTicker.C:
			n, err := svc.Maintain(time.Now().UTC())
			if err != nil {
				log.Error("rate log maintenance failed", "err", err)
				continue
			}
			if n > 0 {
				log.Info("rate log compacted", "files", n)
			}
		}
	}
}

func probeHealth(cfg config.Config) error {
	addr := cfg.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("health probe returned %s", resp.Status)
	}
	return nil
}

func newLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
