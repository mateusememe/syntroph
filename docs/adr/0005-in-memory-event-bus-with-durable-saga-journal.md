# In-memory event bus with durable saga journal

The MVP Event Bus delivers synchronously in-process, while a local append-only Saga Journal persists transitions for recovery and dashboard projection without requiring a broker. Handlers are isolated: one failure becomes saga state and does not block independent handlers. External operations may be delivered at least once and must be idempotent by Event ID.
