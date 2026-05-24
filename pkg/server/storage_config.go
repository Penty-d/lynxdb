package server

import (
	"github.com/lynxbase/lynxdb/pkg/config"
	"github.com/lynxbase/lynxdb/pkg/storage/compaction"
	"github.com/lynxbase/lynxdb/pkg/storage/part"
)

func compactionConfigFromStorageConfig(storageCfg config.StorageConfig) compaction.Config {
	cfg := compaction.DefaultConfig()
	if storageCfg.L0Threshold > 0 {
		cfg.L0Threshold = storageCfg.L0Threshold
	}
	if storageCfg.L1Threshold > 0 {
		cfg.L1Threshold = storageCfg.L1Threshold
	}
	if storageCfg.L2TargetSize > 0 {
		cfg.L2TargetSize = storageCfg.L2TargetSize.Int64()
	}
	if storageCfg.FlushThreshold > 0 {
		cfg.FlushBytes = storageCfg.FlushThreshold.Int64()
	}
	if storageCfg.RowGroupSize > 0 {
		cfg.RowGroupSize = storageCfg.RowGroupSize
	}

	return cfg
}

func batcherConfigFromStorageConfig(storageCfg config.StorageConfig) part.BatcherConfig {
	cfg := part.DefaultBatcherConfig()
	compactionCfg := compactionConfigFromStorageConfig(storageCfg)
	if storageCfg.FlushThreshold > 0 {
		cfg.MaxBytes = compactionCfg.FlushBytes
	}
	if storageCfg.FlushIdleTimeout > 0 {
		cfg.MaxWait = storageCfg.FlushIdleTimeout
	}
	if compactionCfg.L0Threshold > 0 {
		cfg.DelayThreshold = compactionCfg.L0Threshold
		cfg.RejectThreshold = compactionCfg.L0Threshold * 2
	}

	return cfg
}

func compactionWorkersFromStorageConfig(storageCfg config.StorageConfig) int {
	if storageCfg.CompactionWorkers > 0 {
		return storageCfg.CompactionWorkers
	}

	return config.DefaultConfig().Storage.CompactionWorkers
}
