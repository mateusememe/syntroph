# Immutable session diaries and compiled memory

Syntroph will persist an immutable Session Diary for every closed session and relate diaries explicitly. A separately named Compiled Memory may be regenerated or updated from those diaries; it never overwrites their source record. This preserves auditability and replay safety while allowing GitHub Wiki and GitHub Issues to expose different storage semantics.
