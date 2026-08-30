# Structured session artifacts with graph resolution

`syntroph session close` accepts a structured Markdown or JSON Session Artifact from any runtime, with a manual summary fallback; raw transcript capture is excluded from the MVP. Before persisting the resulting Session Diary, Syntroph resolves its mentioned symbols and files through GraphPort and records those references, then updates the graph snapshot with a retriable push.
