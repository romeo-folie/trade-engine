# Git workflow

Every new feature must go out on a dedicated branch and have a PR created before any review or merge. Never push directly to `main` unless the user explicitly requests it.

After pushing a feature branch, always create a PR immediately — never leave code sitting on a remote branch without a PR.

After creating a PR, always start a persistent Monitor for that PR number that watches for reviews and merge/close state changes.

# Memory

After each PR is **merged**, save a `project` memory summarising the feature — what it does, why it was built, and any key implementation details worth knowing in future sessions. Use the standard memory frontmatter format and add a pointer to `MEMORY.md`. Do not write memory at PR creation time; wait for merge.

# PR workflow

After pushing a branch, immediately create a PR and start a persistent Monitor watching that PR for reviews and merge/close events.
