# `terraform/iam-access-analyzer`

Enables IAM Access Analyzer's unused-access finding type for the account, per
`docs/upgrade-research/secrets-management-lifecycle-2026-09-15.md` Finding 5.
A different signal than "this credential leaked" — it flags IAM users/roles
whose own access keys or permissions simply haven't been used recently,
catching *forgotten* credentials, not exposed ones.

## Before applying to a real account

AWS's own `CreateAnalyzer` API reference states "You can create only one
analyzer per account per Region" — read literally, this could mean an
account that already runs an external-access analyzer (`type = "ACCOUNT"`)
in the same region cannot also run this unused-access analyzer
(`type = "ACCOUNT_UNUSED_ACCESS"`) there. AWS's own guidance elsewhere
describes running both analyzer types as complementary and expected, so this
constraint may only apply per-type, not account-wide — but this module does
not resolve that ambiguity for you. Run `terraform plan` against the real
target account first and read AWS's own error message directly if it
conflicts with an existing analyzer, rather than assuming either reading is
correct.

Like every other module in this tree, this is a standalone root module with
its own local state by default — see `../README.md` for the two real
remote-state paths available. Never applied against a real account by this
project's own tooling; `terraform validate`/structural `plan` (no real AWS
credentials) is the extent of this repo's own verification.
