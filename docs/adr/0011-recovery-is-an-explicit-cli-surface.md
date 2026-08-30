# Recovery is an explicit CLI surface

The MVP exposes `syntroph sync status`, `syntroph sync recovery`, `syntroph sync retry [--storage|--graph]`, and `syntroph sync resolve <id> --keep-local|--keep-remote`. Recovery view explains the pending state and proposed next action; conflict resolution always presents a diff and requires an explicit choice.
