# One repository installation per MVP

Each MVP `.syntroph/` installation owns exactly one Git repository, with session identity based on its normalized remote and commit SHA. Cross-repository aggregation is deferred to a later dashboard projection, avoiding ambiguous ownership and synchronization boundaries in the core.
