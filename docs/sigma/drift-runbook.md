# rsigma drift triage

When the nightly drift workflow opens an issue:

1. Open the latest run's `drift.patch` artifact.
2. Decide: (a) bump our supported rsigma range, (b) extend SPL2 to handle the new shape, or (c) document the new shape as unsupported.
3. If (a): run `scripts/sync_rsigma_golden.sh --rsigma-ref <new-tag>`, commit the corpus diff, update `docs/sigma/compat.md`'s supported range.
4. If (b): file a separate issue against the SPL2 area; pin this drift issue until the SPL2 work lands.
5. If (c): edit `docs/sigma/limitations.md` and close the drift issue linking to it.
