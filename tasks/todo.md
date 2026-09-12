# Bug-fix plan — bug_report.md

Working file. Every fix production-grade, with regression tests. `go build && go vet && go test ./...` after each group; `golangci-lint run` at the end.

## Groups

- [x] G1 model+version (Bug 3): `model.PGVersionString(num)` — pre-10 uses `(num/100)%100` minor; delete `pgLower`/`pgVersionShort` duplicates; callers: render/terminal.go, render/advisor.go, cmd/pgbot/{tune,queries,tables,vacuum,erd}.go
- [x] G2 gauges (Bugs 1+2): gauge uses `queryTag` (low 16) + `topLockQuery` ranking; delete `queryHex4`
- [x] G3 statusboard (N2): rows routed through `statusFor` with full governing-finding lists; wraparound += mxid_wraparound; indexes += fk_unindexed; WAL row ← archiving findings
- [x] G4 findings (Bug 4): `archiving_stalled` third arm — never-archived (LastArchivedTime==nil) + walFlowing + uptime > threshold
- [x] G5 waits (Bug 5): `BlockedVictim.LockShare` (of the victim's own samples) populated in waitstudy; waits render prints it; bump WaitsSchemaVersion 1.1.0
- [x] G6 events (Bug 6): sort schemaEvents by (Kind,Object), configEvents by Object
- [x] G7 mcp (Bug 7): `responder` struct centralizes "never reply to a notification"; all handlers use it
- [x] G8 erd (Bug 8+N3): Edge carries From/ToSchema; FKTarget qualified `schema.table.column`; all renderers key by qualified name; display bare unless ambiguous; collapse dead if/else
- [x] G9 why (N1): zero-baseline shift → "slowed from ~0" text, impact scored via documented ceiling, counts as large for confidence
- [x] G10 collect (N4 + health minor): `sampled.Span` per-collector measured window (`rateWindow` fallback); stamps in runner phases; health subtracts own A-commit + failed-poll rollbacks (`OwnTxnFails`)
- [x] G11 prometheus/config (N5+N6): ExpiredFindings sets distinguishing Object; promFamily dedupes label sets (validity is the hard contract); config error names the real interface
- [x] G12 minors: sarif comment, vacuum dead branch, bedrock client mutation, logs authenticated-line narrowing (drop only entries at/after pgbot's own session start, ±2m skew guard)
- [x] G13: full gate — build, vet, golangci-lint, all tests; update bug_report.md fix status
