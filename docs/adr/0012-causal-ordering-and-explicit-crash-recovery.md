# Causal ordering and explicit crash recovery

Core saga transitions follow causal order, while independent handlers may run in parallel after their predecessor is journaled. After a crash, Syntroph presents recovery through `sync recovery` and requires an explicit `sync retry`; it never performs external effects automatically on the next command.
