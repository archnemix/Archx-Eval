package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/config"
	"github.com/archnemix/archx-eval/relay/internal/core"
	"github.com/archnemix/archx-eval/relay/internal/hf"
	"github.com/archnemix/archx-eval/relay/internal/syncctl"
)

// newRemote builds the dataset client described by the configuration.
func newRemote(cfg config.Config) hf.Remote {
	return hf.NewCommitSink(cfg.HFRepo, cfg.HFToken, cfg.HFBaseURL, "main")
}

// restoreState pulls the relay's own subtree back from the dataset before the
// service starts. Without this a Space that has been asleep for an hour comes
// back with no memory of who it has already served.
//
// Only the relay's own prefixes are restored. The question bank is never
// published here, so it is never restored from here either.
func restoreState(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if !cfg.RestoreOnBoot || !cfg.HFEnabled {
		log.Info("dataset restore skipped", "enabled", cfg.HFEnabled, "restore_on_boot", cfg.RestoreOnBoot)
		return nil
	}
	remote := newRemote(cfg)
	if err := remote.(*hf.CommitSink).Ping(ctx); err != nil {
		// A restore failure must not stop the relay from serving; it degrades
		// to "no memory of previous callers", which the logs make obvious.
		log.Error("dataset unreachable; starting with empty state", "err", err)
		return nil
	}

	total := 0
	for _, prefix := range cfg.RestorePrefixes {
		dest := filepath.Join(cfg.DataDir, syncctl.SanitizePath(prefix))
		n, err := syncctl.Restore(ctx, remote, prefix, cfg.DataDir, 8)
		if err != nil {
			log.Error("restore failed for prefix", "prefix", prefix, "dest", dest, "err", err)
			continue
		}
		total += n
		log.Info("restored prefix", "prefix", prefix, "files", n)
	}
	log.Info("dataset restore complete", "files", total)
	return nil
}

// syncToDataset publishes the current state now and exits. Used by -sync.
//
// In production the relay publishes continuously through the controller; this
// one-shot path exists for CI, for a manual push, and for the dry-run mirror
// that keeps everything inside the repository.
func syncToDataset(cfg config.Config, svc *core.Service, log *slog.Logger, dryRun bool) error {
	outDir := os.Getenv("SYNC_OUT_DIR")
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "archx-sync")
	}
	if err := os.RemoveAll(outDir); err != nil {
		return err
	}
	written, err := svc.ExportSnapshot(outDir)
	if err != nil {
		return err
	}
	log.Info("snapshot exported", "files", len(written), "dir", outDir)

	files, err := hf.Collect(outDir, 0)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		log.Info("nothing to publish")
		return nil
	}

	var remote hf.Remote
	switch {
	case dryRun:
		target := os.Getenv("SYNC_MIRROR_DIR")
		if target == "" {
			target = filepath.Join("ci", "outputs", "dataset-mirror")
		}
		log.Info("dry run: mirroring into the repository", "target", target)
		remote = &hf.DirSink{Root: target}
	case cfg.HFEnabled:
		remote = newRemote(cfg)
	default:
		log.Warn("HF_DATA_TOKEN is unset: snapshot stays local", "dir", outDir)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sc := syncctl.New(syncctl.Config{
		Interval:  cfg.SyncInterval,
		MaxFiles:  cfg.SyncMaxFiles,
		MaxQueued: 1 << 20,
	}, remote, log)
	for _, f := range files {
		sc.EnqueueFile(f)
	}

	started := time.Now()
	if err := sc.FlushNow(ctx); err != nil {
		stats := sc.Stats()
		return fmt.Errorf("publish failed: %w (queued=%d published=%d skipped=%d)",
			err, stats.Queued, stats.Published, stats.Skipped)
	}
	stats := sc.Stats()
	log.Info("published",
		"files", len(files),
		"published", stats.Published,
		"skipped_unchanged", stats.Skipped,
		"commits", stats.Commits,
		"duration_s", int(time.Since(started)/time.Second),
		"dry_run", dryRun,
	)
	if stats.Queued > 0 {
		return fmt.Errorf("%d file(s) still queued after flush", stats.Queued)
	}
	return nil
}
