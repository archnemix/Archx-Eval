package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/config"
	"github.com/archnemix/archx-eval/relay/internal/hf"
	"github.com/archnemix/archx-eval/relay/internal/syncctl"
)

// bankPrefix is where the question bank lives inside the dataset.
//
// The bank is the answer key: it carries reference solutions and tests. It
// therefore lives ONLY in the private dataset owned by Archnemix, and is pulled
// into the Space's disk at boot. It is never published by this service (the
// sync controller ignores this prefix), so an upload made here always means
// "the operator changed the bank", never "the relay echoed it back".
const bankPrefix = "bank"

// restoreBank pulls the question bank from the private dataset into BANK_DIR.
//
// A Space's own repository is a public artefact and its disk is wiped on every
// restart, so the bank cannot live in either place. The dataset is the only
// home it has. If this fails the relay still starts and reports itself
// not-ready, which is strictly better than serving guesses.
func restoreBank(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if !cfg.BankFromDataset || !cfg.HFEnabled {
		log.Info("bank restore skipped",
			"bank_from_dataset", cfg.BankFromDataset,
			"hf_enabled", cfg.HFEnabled)
		return nil
	}
	remote := newRemote(cfg)
	if err := remote.(*hf.CommitSink).Ping(ctx); err != nil {
		log.Error("dataset unreachable; the bank will be empty", "err", err)
		return nil
	}

	started := time.Now()
	n, err := syncctl.Restore(ctx, remote, bankPrefix, cfg.BankDir, 4)
	if err != nil {
		return fmt.Errorf("restore bank from %s: %w", bankPrefix, err)
	}
	if n == 0 {
		log.Warn("the dataset holds no bank yet: publish bank/ to the dataset "+
			"or the relay will report not-ready", "prefix", bankPrefix)
		return nil
	}
	log.Info("bank restored from dataset",
		"files", n,
		"dir", cfg.BankDir,
		"duration_s", int(time.Since(started)/time.Second))
	return nil
}

// localBankPresent reports whether BANK_DIR already holds at least one JSONL
// shard, without paying the cost of a full validation pass.
func localBankPresent(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && filepath.Ext(path) == ".jsonl" {
			found = true
			return os.ErrExist
		}
		return nil
	})
	return found
}

// ensureBankDir makes BANK_DIR writable before restore, because a Docker Space
// may give us an image-owned directory the runtime user cannot write to.
func ensureBankDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create bank dir %s: %w", dir, err)
	}
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o640); err != nil {
		return fmt.Errorf("bank dir %s is not writable by uid %d: %w", dir, os.Getuid(), err)
	}
	return os.Remove(probe)
}
