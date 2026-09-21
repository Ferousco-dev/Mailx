# MailX v0.23 Elicitation Plan

## Stakeholders

| Stakeholder | Role | Interest | Influence | Evidence |
| --- | --- | --- | --- | --- |
| Repository owner | commissioner and operator | diagnose mail flow without exposing content | high | milestone specification, 2026-09-21 |
| API developers | primary users | correlate requests with durable email state | medium | existing REST/OpenAPI contracts |
| Mail operators | primary users | diagnose SMTP, queue, delivery, and webhook failures | high | repository runtime paths and Compose |
| Email senders/recipients | affected, not directly consulted | confidentiality of addresses and content | medium | privacy constraints in milestone specification |
| Deployment operators | secondary users | scrape metrics and evaluate readiness | medium | Compose and health endpoints |

## Techniques

1. **Specification analysis:** the supplied v0.23 milestone defines desired outcomes, exclusions, security constraints, and acceptance evidence.
2. **Repository observation:** source, tests, runtime configuration, logs, health routes, queue, worker, SMTP, database, and webhook paths were inspected to identify existing behavior and gaps.

## Findings promoted to requirements

- Existing HTTP access logging is unstructured and lacks authenticated tenant correlation.
- SMTP-only receipt logging emits full message bodies and addresses.
- Worker, dispatcher, delivery, and webhook paths expose only ad-hoc error callbacks.
- Existing liveness is dependency-independent; readiness checks PostgreSQL but not required Redis.
- No metrics endpoint or aggregate counters exist.
- Existing domain IDs provide correlation without adding per-operation random identifiers beyond HTTP request and SMTP session IDs.

## Validation

The repository owner supplied the full milestone contract and answered `assume` for unresolved intake. Assumptions are recorded as DEC-001..DEC-007. Acceptance is automated through the traceable test cases in `.ilana/traceability.csv` and final owner review.
