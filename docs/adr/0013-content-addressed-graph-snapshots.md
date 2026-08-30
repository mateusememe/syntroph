# Content-addressed graph snapshots

Graph snapshots are content-addressed by `graph_snapshot_id`, with a pointer to the active snapshot. The remote orphan branch may be rewritten, but diaries retain the snapshot identity that resolved their references. Code references include repository, commit, path, optional symbol, kind, snapshot ID, and confidence (`authoritative`, `heuristic`, or `unresolved`).
