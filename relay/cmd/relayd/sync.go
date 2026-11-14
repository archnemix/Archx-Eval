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
)

// syncBatchSize keeps each commit comfortably below the API payload limit.
const syncBatchSize = 100

// syncToDataset exports the local store tree and publishes it.
//
// Two modes exist on purpose: production pushes straight to the private
// Hugging Face dataset, while CI uses -dry-run to mirror into a repository
// directory so the artefacts are committed and auditable.
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

	var sink hf.Sink
	switch {
	case dryRun:
		target := os.Getenv("SYNC_MIRROR_DIR")
		if target == "" {
			target = filepath.Join("ci", "outputs", "dataset-mirror")
		}
		log.Info("dry run: mirroring into the repository", "target", target)
		sink = &hf.DirSink{Root: target}
	case cfg.HFEnabled:
		sink = hf.NewCommitSink(cfg.HFRepo, cfg.HFToken, cfg.HFBaseURL, "main")
	default:
		log.Warn("HF_TOKEN is unset: snapshot stays local", "dir", outDir)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	summary := fmt.Sprintf("relay sync: %d files at %s", len(files), time.Now().UTC().Format(time.RFC3339))
	if err := sink.Publish(ctx, summary, files); err != nil {
		return err
	}
	log.Info("published", "files", len(files), "dry_run", dryRun)
	return nil
}
