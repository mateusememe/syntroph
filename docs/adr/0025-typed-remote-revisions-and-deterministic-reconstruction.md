# Typed Remote Revisions and deterministic reconstruction

The Core treats Remote Revision as opaque while providers validate typed forms: `issue:<number>:<updated_at>:<body_hash>` for Issue bodies, `comment:<id>:<updated_at>:<body_hash>` for Remote Corrections, and `git:<commit_sha>` for Wiki pages. A missing Issue binding is reconstructed only during recovery by paginating closed issues carrying both reserved labels and verifying the exact Idempotency Key marker in each body. Wiki reconstruction calculates the deterministic page path and verifies its frontmatter marker.

The stdio MCP process lives for one Syntroph command, negotiates capabilities once, is reused during that command, and shuts down gracefully. It is never a hidden daemon.
