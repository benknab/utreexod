# Public read-only RPC

`--rpcpubliclisten=address:port` enables anonymous HTTP JSON-RPC on a separate
address. It is disabled by default. Repeat the flag to bind multiple addresses.
The public service uses the existing native method parameters and result formats.

## Enable it

On a bridge with a proof index:

```sh
utreexod --flatutreexoproofindex --nobdkwallet \
  --rpcpubliclisten=0.0.0.0:8338
```

Port 8338 is an example, not a default. The port must be explicit. The service
uses the same `--rpccert` and `--rpckey` as authenticated RPC. With the generated
certificate, query it without credentials using:

```sh
curl --cacert /path/to/rpc.cert https://localhost:8338/ \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"1.0","id":1,"method":"getbestblockhash","params":[]}'
```

For an HTTPS reverse proxy on the same host, bind the backend to loopback:

```sh
utreexod --flatutreexoproofindex --nobdkwallet --notls \
  --rpclisten=127.0.0.1:8334 --rpcpubliclisten=127.0.0.1:8338
```

Forward the public hostname to `http://127.0.0.1:8338/`. Apply per-client rate
limits at the proxy. The node applies a shared global budget and does not trust
forwarded client-IP headers.

`--notls` requires all private and public RPC bindings to be localhost. Public
RPC cannot be combined with `--norpc`. The existing authenticated RPC continues
to use credentials or cookie authentication. Credentials sent to the public
address never grant access to extra methods.

## Methods

- `getbestblockhash()` — local tip hash.
- `getblockhash(height)` — hash at a height, including height zero for network
  identification.
- `getblockheader(hash, verbose=true)` — raw header or header details.
- `gettxout(txid, vout, includemempool=true)` — coin amount, script, confirmations,
  coinbase flag, and observed best block. Use `false` for confirmed coin evidence.
- `getutreexoroots(blockhash)` — accumulator roots and leaf count at that block.
- `proveutxochaintipinclusion([txid], [vout], verbosity=1)` — one coin's membership
  proof at the local tip. Verbosity zero returns the native serialized proof;
  verbosity one returns the native JSON result, including `provedathash`.

Hash inputs must contain exactly 64 hexadecimal characters. Output indexes must
fit a uint32. Proof requests must contain exactly one transaction ID and one
output index. Heights must be in the nonnegative int32 range.

All other methods return JSON-RPC error `-32601` (method not found), including
`getutreexoproof`, wallet methods, transaction broadcast, and administrative
methods. This is a separate allowlist from `rpclimituser`.

These methods retain their native node/index requirements. In particular,
selected-coin proofs require `--flatutreexoproofindex` or `--utreexoproofindex`.
The tip is the node's local tip, which may be behind the network during sync.

## Protocol and initial limits

Send HTTP POST requests to `/`. JSON-RPC 1.0 and 2.0 envelopes are accepted;
omitting `jsonrpc` selects 1.0. Parameters use positional arrays. Each batch
member passes the same method and parameter checks as a single request.
Notifications return no result and do not execute these read-only operations.
An explicit null ID in JSON-RPC 2.0 does receive a response.

The initial limits are fixed and shared across all public listen addresses:

- 32 open connections and four active HTTP requests, with no work queue.
- One active membership-proof operation.
- 20 RPC calls per second with a burst of 20; batch items and failed calls count.
- 4 KiB request bodies and at most eight calls per batch.
- 256 KiB per serialized result envelope, at most eight per response.
- 5-second header, 10-second request-read, and 30-second response-write deadlines.
- HTTP connections close after the response. The public service has no WebSocket
  route or subscriptions.

An oversized body returns HTTP 413. Exhausted request capacity returns HTTP 503;
rate exhaustion returns HTTP 429. Both overload responses include `Retry-After: 1`.
A busy proof worker returns JSON-RPC error `-32000`. Malformed parameters return
`-32602`. An oversized result returns `-32603` instead of a truncated result.

Disconnecting or reaching the write deadline does not interrupt native proof
generation. Its worker slot remains occupied until the handler finishes, and
shutdown waits for active public requests before releasing chain resources.
Public request slots are separate from authenticated RPC slots; the services
still share the chain, accumulator locks, CPU, and disk.

## Gathering a membership witness

Call `gettxout` with mempool lookup disabled, then
`proveutxochaintipinclusion`. Fetch the header and roots using the proof's own
`provedathash`. Use `gettxout.bestblock` and its header height when interpreting
the returned confirmations to derive the coin's creation height. Resolve the
creation hash with `getblockhash`.

These calls are not an atomic snapshot. Rebuild the coin's leaf, match it to
`hashesproven`, and verify membership against roots at the returned proof block.
Retry if a tip change or reorganization produces inconsistent evidence. Preserve
native hash byte order and parse uint64 target positions without floating-point
rounding.

A fresh verifier can fetch its public headers and roots through these same
native RPC methods on its chosen node. It does not need the selected outpoint or
the prover's membership witness.
