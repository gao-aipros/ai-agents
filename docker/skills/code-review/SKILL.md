---
name: code-review
description: Review pull requests and code changes with concise, issues-only feedback. Use when user asks for a code review, PR review, "review this PR", "review my changes", or wants feedback on a diff or branch.
---

# Code Review

## Principles

- **Issues only** — never list praise or summaries. The review is exclusively problems and suggestions.
- **Approve when clean** — if there are no meaningful issues, approve with a short note. Do not invent problems to fill categories.
- **Skip empty categories** — only include a category (Correctness, Design, Better ideas, etc.) when you have real findings in it. If no better idea exists, omit the category entirely rather than forcing one.
- **Detailed issues** — each finding includes file/line references, root cause, severity, and a concrete fix.
- **Prefer posting to the PR** — use `gh pr review` or `gh pr comment` so feedback lands on the PR. Fall back to printing if posting isn't possible.
- **Do not run tests** — review for missing test coverage only.
- **Focus on material issues** — correctness, maintainability, and performance. Skip trivial formatting nits unless they violate project conventions.

## Workflow

### 1. Gather the diff

```bash
gh pr diff <number>                            # GitHub PR by number or URL
gh pr list --head <branch> --json number -q '.[0].number' | xargs gh pr diff  # by branch
git diff $(git merge-base origin/HEAD HEAD)..HEAD   # local branch vs merge base (works regardless of default branch name)
```

If the diff is large (>10 files), prioritize files with the most significant changes (largest hunks, core logic, security-sensitive paths) rather than reading every file.

### 2. Read changed files

Read the relevant sections around each change in the diff — not the entire file. For each hunk, read enough surrounding context to understand the logic. Read imported modules only when a change touches their interface. Read callers only when a signature changes.

Read files in parallel when they are independent.

### 3. Review with this checklist

Scan for these categories. Skip any category that has no genuine findings — an empty category is noise.

**Correctness**
- Logic errors, off-by-one, inverted conditions, missing null/error handling
- Race conditions, async ordering, stale closures
- Security: injection, auth bypass, exposed secrets, input validation gaps

**Design**
- Unnecessary complexity or abstraction that could be simpler
- Missing separation of concerns, tangled responsibilities
- Duplicated logic that should be shared
- Violations of existing codebase patterns or conventions

**Missing tests**
- New logic without corresponding test cases
- Edge cases the tests don't cover (empty input, boundary values, error paths)
- Integration points that need tests (API calls, DB queries, external services)

**Better ideas**
- A simpler algorithm, data structure, or library function that replaces the current approach
- An existing utility or helper in the codebase that the author may have missed
- A more idiomatic pattern for the language/framework

**Nits**
- Misleading names, dead code, leftover comments, stray logs
- Type annotations that are too loose or too tight
- Error messages that don't help the reader diagnose the problem

### 4. Format the review

Each issue follows this template:

```
**[Severity] `file:line`** — one-line summary
> what's wrong, why it matters, concrete fix
```

Severities: `BLOCKER` (must fix before merge), `IMPORTANT` (should fix), `NIT` (nice to fix).

For cross-cutting issues spanning multiple files, use `**Multiple files** — summary` with a list of affected files.

Always tag each issue with its category: `[Correctness]`, `[Design]`, `[Missing test]`, `[Better idea]`, or `[Nit]`.

Write the output to `review.md`.

### 5. Post to PR

```bash
gh pr review <number> --comment --body "$(cat review.md)"
# Use --request-changes only when there are BLOCKER issues:
gh pr review <number> --request-changes --body "$(cat review.md)"
# Or use gh pr comment for individual comments:
gh pr comment <number> --body "$(cat review.md)"
```

If `gh` is not authenticated, use `gh auth status` to check and ask the user to authenticate. Fall back to printing the review if posting isn't possible.

## Approving a clean PR

If no issues are found after a thorough review, approve with:

```bash
gh pr review <number> --approve --body "LGTM. No issues found."
```
