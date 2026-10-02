# minidb — a database written to understand real databases

*English summary of [README.md](./README.md). All diaries, blog posts and docs are in Vietnamese.*

A mini relational database in Go, built from `pread`/`fsync` up to `EXPLAIN`, with one rule:
**every phase ends with a crash test or a benchmark that proves understanding**, not with "it runs".

No database libraries. Go stdlib + syscalls only. ~32k lines of Go, 191 tests, 11 fuzz targets,
10 phases, 13 blog posts, 48 commits (Sept 1 → Oct 2, 2026).

## What I built, and how each layer is proven

| Layer | Proof |
|---|---|
| **Pager** — file as an array of 4 KB pages, dual meta page + crc32c, freelist | 40 simulated crash points, file reopens valid every time |
| **Slotted page** — variable-length records, compaction | 1.42M fuzz iterations, no invariant broken |
| **Buffer pool** — pin/unpin, LRU / CLOCK / LRU-K | hit ratio on zipfian + sequential flooding, compared to Belady |
| **B+Tree** — split, merge, redistribute, cursor | 7-invariant property test; 1M keys: random inserts cost **33x** the page writes of sequential |
| **WAL + ARIES-lite** — analysis/redo/undo, fuzzy checkpoint, group commit | **200/200 random `kill -9`**, 9,194 committed txns verified. Plus a *negative test*: deliberately drop the log and the test **must fail**, 10/10 |
| **MVCC + S2PL** — snapshots, 4 isolation levels, deadlock via wait-for graph | 5 anomalies × 4 levels match theory cell by cell, in **both** directions |
| **Index + planner** — order-preserving keys, multi-table catalog in one tree, cost model | 3 plans, same result; index break-even selectivity **measured**, not quoted |
| **SQL** — lexer/parser/binder, logical vs physical plan, Grace hash join (spills to disk), external merge sort, predicate pushdown | two execution paths agree under `-race`; parser fuzz: must terminate, print-reparse must round-trip |
| **Against real DBs** — same questions on Postgres 17 / MySQL 8.4 / MariaDB 11.8 | 11 tables of numbers, several refuting my own earlier conclusions |

## Where I was wrong, and what the numbers caught

1. **Index break-even measured at 36.6%**, textbooks say 5–20%. My explanation: "minidb has no real I/O".
   Phase 9 ran Postgres / MySQL / MariaDB in RAM too, 1M rows: break-even **11.7 / 6.6 / 6.1%**.
   Explanation refuted. Real cause: minidb's per-row *scan* cost is much higher while *lookup* cost matches InnoDB.
2. **Profiling that debt**: the cost is allocation, not byte copying. Zero-copy `GetFunc` API, 16 A/B pairs on
   bare Linux: seq scan **404 → 357 ns/row**, allocs 140k → 40k. Break-even 41% → 36.5%, still **not** below the
   15% target. Right fix, not enough of it; the rest is B+Tree cursor re-pinning.
3. **A missing optimizer rule.** Predicate pushdown gave 1.18x, too little. Missing: inferring predicates across
   equi-joins. With the rule: **18x**. First time measurement found something *absent*, not something wrong.
4. **"MariaDB purges 40x faster than MySQL"** was a **counter artifact**: MySQL only decrements `history_list` on
   rollback-segment truncation. Three hypotheses written down *before* running; the first two were refuted.
5. **MySQL allows lost update at REPEATABLE READ**, Postgres does not. UUIDv4 primary keys write **26–32x**
   more pages on InnoDB but only **~1.3x** on Postgres (non-clustered heap). Same names, different semantics.

Full record in [`diary/`](./diary) and [`docs/debts.md`](./docs/debts.md), a ledger of known gaps where every
entry has the command to settle it. 17 settled, 57 open.

## Architecture

```
 cmd/minidb REPL
 internal/sql      lexer → parser → AST
 internal/plan     binder → logical rewrite → physical planner (cost model)
 internal/exec     Volcano iterators: SeqScan, IndexScan, NestedLoop, GraceHashJoin, ExternalSort
 internal/query    seq / index / index-only plans + selectivity estimation
 internal/table    catalog; all tables + indexes in ONE B+Tree keyed by oid prefix
 internal/keys     order-preserving key encoding: composite, ASC/DESC, canonical
 internal/txn      MVCC: version chains, snapshots, 4 isolation levels, vacuum
 internal/lock     S2PL: S/X, point + range locks, deadlock detection
 internal/db       Begin/Commit/Abort, fuzzy checkpoint, 3-pass ARIES recovery
 internal/wal      log records + crc32c, block diffs, LSN = byte offset, group commit
 internal/btree    node = 1 page, split / merge / redistribute, cursor
 internal/bufpool  pin/unpin, dirty tracking, LRU / CLOCK / LRU-K, WAL-before-data hook
 internal/page     slotted page
 internal/pager    4 KB pages, dual meta page + crc32c, freelist; pread / pwrite / fsync
```

B+Tree + WAL, update-in-place (the Postgres / InnoDB / SQLite family). LSM is future work.

## Try it in 5 minutes

Go 1.26+, Linux. Phase 9 also needs Docker.

```bash
make test               # all unit + property tests
make repl               # SQL REPL
make crashlab           # 20 random kill -9, then verify durability
make crashlab-nowrite   # MUST FAIL: proves the crash test can detect a broken implementation
make txnlab-anomaly     # anomaly × isolation table, exit 1 on any mismatch with theory
make sqllab-join        # nested loop vs hash join crossover
```

## Limits, stated plainly

- Not for production. No network, no auth, one file, one process.
- README numbers were measured on bare Linux (AMD Ryzen 7 H 255, 2026-10-01); most diary tables were measured on
  WSL2 earlier. Ratios hold, absolute numbers differ.
- Real torn writes not yet reproduced (`kill -9` cannot produce them). `dm-flakey` tooling is built, not yet run.
- Open debts in [`docs/debts.md`](./docs/debts.md), each with a command to settle it.
