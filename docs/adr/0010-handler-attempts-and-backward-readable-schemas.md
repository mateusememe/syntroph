# Handler attempts and backward-readable schemas

Event envelopes remain immutable and carry `event_id`, `type`, `occurred_at`, `repository_id`, `saga_id`, `correlation_id`, `causation_id`, `schema_version`, and `payload`. Each handler attempt is recorded separately with `attempted_at`, `handler_id`, outcome, and error. Schema versions increase per event type; readers support the current and immediately previous version through read-time migration without rewriting journals or immutable diaries.
