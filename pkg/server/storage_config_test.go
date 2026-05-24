package server

import (
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/config"
	"github.com/lynxbase/lynxdb/pkg/storage/compaction"
)

func TestCompactionConfigFromStorageConfigUsesConfiguredThresholds(t *testing.T) {
	storageCfg := config.DefaultConfig().Storage
	storageCfg.L0Threshold = 12
	storageCfg.L1Threshold = 7
	storageCfg.L2TargetSize = 3 * config.GB
	storageCfg.FlushThreshold = 256 * config.MB
	storageCfg.RowGroupSize = 12345

	cfg := compactionConfigFromStorageConfig(storageCfg)
	if cfg.L0Threshold != 12 {
		t.Fatalf("L0Threshold: got %d, want 12", cfg.L0Threshold)
	}
	if cfg.L1Threshold != 7 {
		t.Fatalf("L1Threshold: got %d, want 7", cfg.L1Threshold)
	}
	if cfg.L2TargetSize != int64(3*config.GB) {
		t.Fatalf("L2TargetSize: got %d, want %d", cfg.L2TargetSize, int64(3*config.GB))
	}
	if cfg.FlushBytes != int64(256*config.MB) {
		t.Fatalf("FlushBytes: got %d, want %d", cfg.FlushBytes, int64(256*config.MB))
	}
	if cfg.RowGroupSize != 12345 {
		t.Fatalf("RowGroupSize: got %d, want 12345", cfg.RowGroupSize)
	}
}

func TestCompactionConfigFromStorageConfigKeepsDefaults(t *testing.T) {
	cfg := compactionConfigFromStorageConfig(config.StorageConfig{})
	defaults := compaction.DefaultConfig()

	if cfg.L0Threshold != defaults.L0Threshold {
		t.Fatalf("L0Threshold: got %d, want %d", cfg.L0Threshold, defaults.L0Threshold)
	}
	if cfg.L1Threshold != defaults.L1Threshold {
		t.Fatalf("L1Threshold: got %d, want %d", cfg.L1Threshold, defaults.L1Threshold)
	}
	if cfg.L2TargetSize != defaults.L2TargetSize {
		t.Fatalf("L2TargetSize: got %d, want %d", cfg.L2TargetSize, defaults.L2TargetSize)
	}
}

func TestBatcherConfigFromStorageConfigUsesConfiguredFlushSettings(t *testing.T) {
	storageCfg := config.DefaultConfig().Storage
	storageCfg.FlushThreshold = 256 * config.MB
	storageCfg.FlushIdleTimeout = 15 * time.Second

	batcherCfg := batcherConfigFromStorageConfig(storageCfg)
	if batcherCfg.MaxBytes != int64(256*config.MB) {
		t.Fatalf("MaxBytes: got %d, want %d", batcherCfg.MaxBytes, int64(256*config.MB))
	}
	if batcherCfg.MaxWait != 15*time.Second {
		t.Fatalf("MaxWait: got %s, want 15s", batcherCfg.MaxWait)
	}
}

func TestBatcherConfigFromStorageConfigUsesCompactionPressureThreshold(t *testing.T) {
	storageCfg := config.DefaultConfig().Storage
	storageCfg.L0Threshold = 42

	batcherCfg := batcherConfigFromStorageConfig(storageCfg)
	if batcherCfg.DelayThreshold != 42 {
		t.Fatalf("DelayThreshold: got %d, want 42", batcherCfg.DelayThreshold)
	}
	if batcherCfg.RejectThreshold != 84 {
		t.Fatalf("RejectThreshold: got %d, want 84", batcherCfg.RejectThreshold)
	}
}

func TestCompactionWorkersFromStorageConfigUsesConfiguredWorkers(t *testing.T) {
	storageCfg := config.DefaultConfig().Storage
	storageCfg.CompactionWorkers = 6

	if got := compactionWorkersFromStorageConfig(storageCfg); got != 6 {
		t.Fatalf("workers: got %d, want 6", got)
	}
}
