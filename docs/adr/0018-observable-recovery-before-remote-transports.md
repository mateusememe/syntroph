# Observable recovery before remote transports

Remote integration starts by extending the Saga Journal and Recovery View to durably project Remote Binding, provider, remote identity and revision, errors, conflict metadata, and content required for a user-visible diff. REST, Git Wiki, and MCP transports are implemented only after this recovery contract is testable.

`.syntroph/config.yaml` is the sole configuration format before the first stable release; the existing JSON prototype receives no compatibility path. A GitHub Wiki without its required initial page records Storage Prerequisite Missing and presents actionable setup instructions instead of attempting administrative bootstrap. The explicit MCP client connects to a configured endpoint or command, validates required tools, and leaves authentication entirely to the MCP process.
