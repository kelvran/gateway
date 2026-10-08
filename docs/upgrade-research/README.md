# Upgrade research — engineering record, not user documentation

Dated research reports, `<topic>-YYYY-MM-DD.md`, each ending in verdicts (`build_now`, `not_yet`, `never`, `deferred`) for a point in time. They are proposals and evidence, not descriptions of the system. Later reports flip earlier verdicts; the newest report on a topic wins over an older one, and a dated entry in [`DECISIONS.md`](../../DECISIONS.md) wins over any report.

Paths named inside these reports may never have been created, by design: a report proposes a file, and the decision may have gone another way. That is why this directory is excluded from `scripts/check-doc-paths.sh`, the CI check that every other `docs/…` citation in the repository resolves. Do not "fix" a path here; the report is a record of what was proposed.

For what the gateway does today, start at [`docs/README.md`](../README.md).
