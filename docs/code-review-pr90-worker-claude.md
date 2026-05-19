**[IMPORTANT] `docker/worker-claude/Dockerfile:43`, `docker/copilot/Dockerfile:42`, `docker/opencode/Dockerfile:42`, `docker/codex/Dockerfile:51` — Unnecessary second clone per build** [Design]

The code-review skill is fetched via a separate `git clone` from a different repo, adding network dependency and build time. If `code-review` exists in `mattpocock/skills` (the same upstream repo the other skills come from), it should be added to the existing `for skill in ...` loop — a one-line addition instead of 3 lines duplicated across 4 Dockerfiles. If it only lives in `Noodle05/skills`, consider contributing it upstream to eliminate the dependency on a personal fork.

**[NIT] `docker/master-agent/CLAUDE.md:16` — Inconsistent skill reference format** [Design]

The master-agent file lists skills with `/skill-name` slash-prefix format (line 11) but the new guidance line uses bare names: "use `grill-with-docs`". Use `/grill-with-docs` to match the file's own convention.
