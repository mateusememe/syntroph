# Private conflict snapshots and per-diary locks

Remote Bindings store only identity, URL, typed Remote Revision, and hashes. Divergent remote bodies are written with mode `0600` to `.syntroph/storage/conflicts/<idempotency_key>/<revision>.remote.md` and referenced from the Saga Journal, enabling offline diffs without treating remote content as operational binding state.

Every remote effect acquires a Mirror Lock by Idempotency Key. Concurrent attempts return already-in-progress without creating duplicate pending obligations. Lock ownership records PID and start time; an orphan is cleared only by explicit recovery after verifying that its process no longer exists.
