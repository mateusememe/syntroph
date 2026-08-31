# Append-only Issue conflict resolution

GitHub Issues does not document atomic compare-and-swap for body updates, so Syntroph never represents a best-effort PATCH as safe conflict resolution. `--keep-local` adds an append-only Remote Correction comment and advances the Remote Binding to that effective version; `--keep-remote` explicitly accepts the edited remote version. Existing Issue bodies are not overwritten during resolution.
