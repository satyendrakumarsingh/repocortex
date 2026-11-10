# Candidate business capabilities

DERIVED from the graph, not inferred from it. A capability here is a DECLARED component named by what the index says it exposes, what it touches, and what it needs from the other capabilities. Nothing below reads an identifier and decides what business the system is in; where the graph is silent, this document says so instead of filling the gap.

Three claims a per-capability listing cannot make on its own are made separately, because each is the kind of thing a reader would otherwise assume: how much of the index the model accounts for, which tables more than one capability reaches, and which endpoints and tables it accounts for not at all.

Index run 7: 4 files, 11 symbols, 8 edges — of which 2 EXPOSES, 3 table read/write, 1 IMPLEMENTS/EXTENDS. 3 endpoints and 2 tables are declared.

4 candidate capabilities, one per declared component.

| Capability | Endpoints | Tables | Depends on | Files | Symbols |
| --- | ---: | ---: | ---: | ---: | ---: |
| api | 3 | 2 | 2 | 1 | 5 |
| store | 0 | 1 | 1 | 2 | 4 |
| service | 0 | 0 | 1 | 1 | 2 |
| frontend | 0 | 0 | 0 | 0 | 0 |

The capabilities above account for 3 of 3 endpoints and 2 of 2 tables in this index.

## api

Declared in `.cortex.yml` as `internal/api/**`.

### Exposes

| Endpoint | Cited at |
| --- | --- |
| GET /users | internal/api/users.go:10 (source_code, high) |
| POST /users | internal/api/users.go:20 (source_code, high) |
| GET /health | internal/api/users.go:25 (source_code, high) |

### Owns or uses

| Table | Access | Through | Cited at |
| --- | --- | --- | --- |
| audit_log | writes | CreateUser | internal/api/users.go:46 (source_code, low) |
| users | writes | CreateUser | internal/api/users.go:44 (source_code, low) |

Access is what the graph records, not ownership. A table this component writes and another component also writes is shared; see "Shared data" below.

### Depends on

| Capability | Edges | Kinds | Verdict | Cited at |
| --- | ---: | --- | --- | --- |
| store | 2 | WRITES | forbidden (api-goes-through-service) | internal/api/users.go:44 (source_code, low) |
| service | 1 | CALLS | allowed | internal/api/users.go:32 (source_code, high) |

These are the component crossings the graph records, in the same order the architecture document lists them. A dependency the declared rules forbid is named as such: it is still what the code does, which is the point of deriving this rather than writing it.


## store

Declared in `.cortex.yml` as `internal/store/**, migrations/**`.

### Exposes

No HTTP endpoint in this index belongs to this component.

### Owns or uses

| Table | Access | Through | Cited at |
| --- | --- | --- | --- |
| users | reads | Query | internal/store/users.go:17 (source_code, high) |

Access is what the graph records, not ownership. A table this component writes and another component also writes is shared; see "Shared data" below.

### Depends on

| Capability | Edges | Kinds | Verdict | Cited at |
| --- | ---: | --- | --- | --- |
| service | 1 | IMPLEMENTS | allowed | internal/store/users.go:8 (source_code, high) |

These are the component crossings the graph records, in the same order the architecture document lists them. A dependency the declared rules forbid is named as such: it is still what the code does, which is the point of deriving this rather than writing it.


## service

Declared in `.cortex.yml` as `internal/service/**`.

### Exposes

No HTTP endpoint in this index belongs to this component.

### Owns or uses

No table read or write in this index belongs to this component.

### Depends on

| Capability | Edges | Kinds | Verdict | Cited at |
| --- | ---: | --- | --- | --- |
| store | 1 | CALLS | allowed | internal/service/user.go:14 (source_code, medium) |

These are the component crossings the graph records, in the same order the architecture document lists them. A dependency the declared rules forbid is named as such: it is still what the code does, which is the point of deriving this rather than writing it.


## frontend

Declared in `.cortex.yml` as `web/**`.

**The graph is silent about this component.** It matches 0 files and 0 symbols, but this index attributes no HTTP endpoint, no table access and no dependency on another declared component to it. That is not evidence it does nothing — it may be a library, a front end, or code whose framework this indexer does not recognise — so nothing is claimed about it here.

## Shared data

1 table is reached by more than one capability. Two capabilities writing one table are coupled through the database whatever the component boundaries say, so this is the section to read before moving either of them.

| Table | Capability | Access | Cited at |
| --- | --- | --- | --- |
| users | api | writes | internal/api/users.go:44 (source_code, low) |
| users | store | reads | internal/store/users.go:17 (source_code, high) |

## Not accounted for by any capability

Every endpoint and every table in this index is accounted for.

---

Nothing was omitted: every section fitted inside its cap.
