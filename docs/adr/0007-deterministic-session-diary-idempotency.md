# Deterministic session diary idempotency

Syntroph derives a Session Diary idempotency key from `repository_id`, commit SHA, and the Session Artifact hash. Resubmission of the same key returns the existing diary, while a different artifact at the same SHA remains a distinct diary; technical at-least-once delivery cannot duplicate knowledge.
