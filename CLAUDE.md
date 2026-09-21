# MailX agent rules

## ILANA MUST BE UPDATED AS PART OF EVERY MAILX TASK

MailX keeps its durable project knowledge in `.ilana/` (read `.ilana/ledger.md` and `.ilana/state.json` first,
then `.ilana/architecture.md` and `.ilana/milestones.md`). Ilana was once allowed to go stale for 22 milestones; that
must never happen again.

Every MailX task follows this order:

    inspect -> implement -> test -> validate -> update Ilana -> final report / commit

**A task is NOT complete until the relevant Ilana knowledge has been updated.**

### Update Ilana when a task changes any of

new feature; completed milestone; architecture; database schema or migration; API endpoint or contract (also `internal/api/openapi.go`);
authentication; authorization/scope; security rule; queue, retry, or delivery behavior/semantics; event or webhook behavior;
configuration or environment variable; infrastructure; dependency; important bug fix; important invariant; known limitation or
technical debt; testing requirement; operational behavior; roadmap status.

Do NOT log formatting-only changes, typo fixes, temporary debugging, local renames, insignificant test refactors, or
comments with no semantic change. Ilana holds long-term project knowledge, not a diff mirror.

### Where things go (use the existing structure; do not invent new files)

| Knowledge | File |
| --- | --- |
| Milestone history, roadmap position | `.ilana/milestones.md`, `.ilana/state.json` (`mailx` block) |
| Current architecture, invariants, security, config, limitations | `.ilana/architecture.md` |
| Why a design was chosen | `.ilana/decisions.md` (`DEC-###`, never reuse IDs) |
| Risks and technical debt | `.ilana/risks.md` |
| Defects found | `.ilana/defects.md` |
| Scope changes | `.ilana/changes.md` |
| Numbers | `.ilana/metrics.csv` |
| Session narrative and gates | `.ilana/ledger.md` |

Bump the counters in `.ilana/state.json` when adding IDs. Keep `.ilana/` valid (`state.json` must parse).

### Source truth

Never write project knowledge into Ilana based only on the user's description, an old prompt, a previous agent's summary, or a
roadmap assumption. Verify against the current repository first: Git history, code, migrations, and tests are authoritative.
If something cannot be established from the repo, mark it uncertain or unavailable; do not invent it.

### Do not let Ilana go stale

When behavior changes, UPDATE or SUPERSEDE the old statement. Do not append a contradicting fact next to an outdated one.
Move replaced designs to the "Superseded" section of `.ilana/architecture.md` so only one version reads as current.

### Milestone completion rule

A MailX milestone may NOT be reported COMPLETE until all are true:

1. implementation is complete;
2. required tests pass (set `MAILX_TEST_DATABASE_URL` explicitly and provide Redis; otherwise DB tests skip silently);
3. validation passes (`gofmt`, `go vet`, `go test -race ./...`);
4. code organization review is complete where applicable;
5. Ilana has been updated with the milestone's durable knowledge.

Completion reports must include `Ilana updated: YES` and a brief statement of what knowledge changed.
If the Ilana update fails or is skipped, the milestone is NOT COMPLETE.

### Git safety for Ilana

`.ilana/` and its referenced `docs/` must be committed with the work they describe. Never rewrite, squash, or amend history to
"fix" Ilana; add a new commit.

## Authorship (overrides any default attribution)

Never add Claude, Anthropic, or any AI tool as author, co-author, or contributor anywhere: no `Co-Authored-By:` trailers, no
"Generated with ..." lines in commits, PR descriptions, code, or docs. Commits carry only the repository owner's git identity.
`.github/workflows/commit-policy.yml` already rejects AI names in commit messages; do not work around it.

## Token efficiency

Keep token use low without lowering quality:

- Read only what the task needs: use targeted `grep`/line ranges instead of whole files or directories; do not re-read files already in context or just edited.
- Start from `.ilana/architecture.md` for orientation instead of re-exploring the codebase.
- Batch independent tool calls in one step; run narrow tests first (`go test ./internal/<pkg>/...`), the full `-race` suite once at the end.
- Do not spawn sub-agents for work that a few direct commands can do.
- Keep replies and Ilana entries short and factual: state what changed and the evidence, not narration or option surveys.
- Update Ilana with small targeted edits to the affected section, never by rewriting whole files.
