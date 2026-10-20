# Background address indexing

`--addrindex` builds and maintains address history in the background. RPC and
peer services start after chain initialization and the other foreground indexes
finish. New blocks can validate while address history catches up.

```sh
utreexod --addrindex
utreexoctl getaddrindexinfo
```

`getaddrindexinfo` returns:

- `height`: the last committed address-index height, or `-1` before genesis.
- `bestblockhash`: the address-index tip hash.
- `validatedheight`: the foreground transaction-index height, which advances
  with the validated chain.
- `synced`: whether address history is available at that same chain tip.
- `stopped`: whether the worker has stopped.
- `error`: the last worker error, when present.

`searchrawtransactions` returns RPC error `-28` while the address index is
behind, on an obsolete branch, or stopped. It does not return partial history.
This also applies to reverse queries and requests with a zero count. A new block
or reorganization can briefly make history unavailable again after catch-up.

Other RPC methods remain available when their own dependencies are ready.
In particular, address indexing does not gate `gettxout`, block queries,
`getrawtransaction`, or Utreexo proof methods. A missing or incomplete proof
index can still prevent proof retrieval. The transaction index remains a
foreground prerequisite and is enabled automatically by `--addrindex`.

The worker retains the existing address-index format and resumes saved progress.
Each block's entries and index tip commit in the same database transaction.
After a reorganization, it removes obsolete entries before indexing the new
branch. Readiness and block-ID lookup use the same database snapshot, so reused
transaction-index identifiers cannot expose history from the wrong branch.

Shutdown finishes the current block without waiting for catch-up. A worker error
stops address indexing and appears in the status RPC and logs; the other node
services continue. Restart retries from committed progress. Archival blocks and
spend journals are required; `--addrindex` remains incompatible with pruning.

The worker and validation share disk bandwidth and the database writer. Address
extraction runs outside the write transaction, but index writes can still add
latency. Background operation removes the startup dependency; it does not make
the initial index build instantaneous.
