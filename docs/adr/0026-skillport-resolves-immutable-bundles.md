# SkillPort resolves immutable runtime-neutral bundles

SkillPort is a catalog and resolver, not a prompt executor. It validates and prepares an immutable, runtime-neutral Skill Bundle; RuntimePort adapters remain responsible for translating that bundle into prompts, temporary files, or native runtime mechanisms. This keeps the Core independent from Claude, Codex, and Antigravity conventions, prevents SkillPort from writing into runtime-owned directories, and gives every runtime adapter the same small interface and contract suite.
