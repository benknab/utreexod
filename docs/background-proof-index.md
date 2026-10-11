# Background proof indexing

An archival bridge node can validate and download blocks independently of
Utreexo proof production:

```sh
utreexod --flatutreexoproofindex --backgroundproofindex --prune=0
utreexoctl getproofindexinfo
utreexoctl setproofindexpaused true
utreexoctl setproofindexpaused false
```

`--proofindexpaused` starts the worker paused. `--proofindexdelay=100ms` inserts
a delay between proof blocks to reduce competition for disk and CPU. The existing
`--utreexoproofindexmaxmemory` sets its cache flush threshold. Pause requests take
effect after the current block; recovery and checkpoints finish before pausing.

The validated chain and proof index have separate tips. Archived blocks and
spend journals form the backlog; there is no unbounded in-memory queue. The
worker uses the synchronous per-block proof algorithm. A proof error stops the
worker and is reported in `getproofindexinfo` and the log while validation
continues. Restart retries recovery from the saved checkpoint.

Historical proof requests are bound to the indexed block's hash and return an
unavailable error while the worker is busy, recovering, or on an obsolete fork.
Current-state proofs require the proof and validation tips to match. New peers
are advertised Utreexo services only when those tips match; ordinary Bitcoin
block services remain available while proofs lag.

Shutdown finishes the current operation and saves the **proof tip**, without
waiting for catch-up. Existing flat-file and accumulator formats are retained;
an additive database replay-intent record covers interrupted writes and reorgs.
After an unclean stop, restart in background mode to finish recovery. After a
successful clean stop, synchronous mode can reuse the saved index (and catches
up before starting the node). Background mode requires unpruned source data and
cannot be combined with the database-backed synchronous proof index.

Use this build's synchronous mode as the fallback when proofs lag. Older builds
with the legacy flat-index startup pipeline can write incorrect per-height roots
during catch-up; that pipeline is no longer used by this build.
