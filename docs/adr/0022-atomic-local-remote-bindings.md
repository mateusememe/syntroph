# Atomic local Remote Bindings

The current projection for each mirror is written atomically to `.syntroph/storage/bindings/<idempotency_key>.json`, which remains outside the working branch. The Saga Journal is the auditable history and can reconstruct the projection. Bindings are the normal lookup path; deterministic remote markers support recovery discovery only.
