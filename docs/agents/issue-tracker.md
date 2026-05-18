# Issue Tracker

This project uses GitHub Issues via the `gh` CLI.

## Conventions

- **Create**: `gh issue create --title "..." --body "$(cat <<'EOF' ... EOF)"`
- **Read**: `gh issue view <number> --comments`
- **List**: `gh issue list --label "..." --state open --json title,number,labels --jq '.[]'`
- **Comment**: `gh issue comment <number> --body "..."`
- **Labels**: `gh issue edit <number> --add-label "..."` / `--remove-label "..."`
- **Close**: `gh issue close <number> --comment "..."`

`gh` infers the repo from the git remote when run inside a clone.

## Vocabulary

- "publish to the issue tracker" → create a GitHub issue
- "fetch the relevant ticket" → `gh issue view <number> --comments`
