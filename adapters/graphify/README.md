# Graphify adapter spike

This package is an optional `GraphPort` adapter. Syntroph's core and local
graph adapter do not depend on the Graphify binary or its Go modules. Enable
this adapter only when a compatible `graphify` executable is installed.

The spike invokes:

```text
graphify graph export --format json --repository <repository_id> --commit <commit_sha>
```

The command must write a JSON object containing `graph_snapshot_id` and either
`references` (`path`, optional `symbol` and `kind`) or `nodes` (`path` or
`file`, optional `symbol`/`name` and `kind`). A missing executable, non-zero
exit, or unavailable repository produces an unavailable error; malformed or
identity-mismatched JSON produces an incompatible error. SessionCloser maps
either condition to `GraphResolutionPending` and keeps the diary durable.

The command and JSON shape are intentionally isolated in this spike because
Graphify's CLI contract may change. A future adapter release can update this
boundary without changing `GraphPort` or the Core.
