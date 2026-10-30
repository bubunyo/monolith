# Issues

Detailed reports for non-trivial bugs, regressions, and platform anomalies encountered
during development.

Each report follows the structure:

1. **Report** — what was observed and reported, verbatim symptom
2. **Environment** — host, binary, cache state at the time
3. **Tests run during diagnosis** — every command and what it returned
4. **Assumptions made and how they were validated or refuted** — the explicit hypothesis table
5. **Diagnosis** — what was actually wrong (may be multiple distinct problems)
6. **Solution / mitigation shipped** — what code changed, what it fixes, what it doesn't
7. **Caveats** — what could still go wrong, what assumptions the fix makes
8. **Follow-up tasks** — open work
9. **References** — files touched, related docs, reproducer commands

## Index

| # | Title | Status |
|---|-------|--------|
| [0001](./0001-vm-run-appears-to-hang.md) | `vm run` appears to hang at the boot banner | Resolved (vz fork bug; moved to vz v4) |
