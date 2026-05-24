package compaction

// Config is the single normalized configuration path for compaction planning.
type Config struct {
	L0Threshold  int
	L1Threshold  int
	L2TargetSize int64
	FlushBytes   int64
	RowGroupSize int
}

// DefaultConfig returns compaction defaults matching the historical constants.
func DefaultConfig() Config {
	return Config{
		L0Threshold:  L0CompactionThreshold,
		L1Threshold:  L1CompactionThreshold,
		L2TargetSize: L2TargetSize,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.L0Threshold <= 0 {
		c.L0Threshold = d.L0Threshold
	}
	if c.L1Threshold <= 0 {
		c.L1Threshold = d.L1Threshold
	}
	if c.L2TargetSize <= 0 {
		c.L2TargetSize = d.L2TargetSize
	}

	return c
}
