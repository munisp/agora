# SPEC-W31 — Next-Generation Graph Roadmap (Declared, Not Scheduled)

**Status:** roadmap contract · **Date:** 2026-08-05
This SPEC declares the post-W30 graph waves and their trigger conditions. Each item becomes a full build SPEC only when its trigger fires. Nothing here is built in W29/W30.

## R1 — Agentic Outreach Copilot
LLM agent that plans multi-step campaigns over the graph: proposes segment → message sequence → channel mix → pacing, with human approval at each step; executes through existing audience/notification APIs. Builds on W28 ask (template selection) + W29 scores.
**Trigger:** activation-credit revenue proves tenants pay for audiences; ≥3 managed-service campaigns delivered manually.
**Why next-gen:** turns the platform from "tool operators drive" to "analyst that proposes, human disposes."

## R2 — Community Detection & Influence Mapping
Offline Louvain/Leiden over referral+contact+messaging edges per tenant → community_id write-back; campaign use: ward-level influence clusters, key-influencer identification (eigenvector centrality on REFERRED/MESSAGED). SMB use: organic customer tribes for lookalike expansion.
**Trigger:** campaign tenants with ≥50k-person graphs (communities need density to be meaningful).

## R3 — Temporal Graph Analytics
Time-sliced graph snapshots in the lakehouse; trend detection (community drift, churn-wave early warning, campaign momentum by ward). Extends W28 export jobs with snapshot_day partitions (already partitioned — query layer is the work).
**Trigger:** ≥90 days of trajectory data accumulated.

## R4 — Federated Benchmark Aggregates
k-anonymized cross-tenant benchmark product (monetization doc §2.7): per-industry medians for recall uplift, consent rates, churn distributions. SQL-enforced minimum cohort (k≥20), no person-level data, tenant opt-in flag.
**Trigger:** ≥20 active tenants in one vertical (else k-anonymity unachievable).

## R5 — Campaign Simulation (Graph Digital Twin)
Agent-based simulation on a tenant's voter/customer graph: "if we contact this 20k consented audience with message A vs B, projected turnout/conversion" — Monte Carlo over propensity distributions from W29.
**Trigger:** W29 propensity scores validated against realized outcomes (calibration report showing Brier score < 0.2).

## R6 — Real-Time Scoring & Streaming Features
Move W29 batch scoring to streaming (Kafka → feature updates → sub-minute score refresh) for high-velocity tenants.
**Trigger:** any tenant where batch latency measurably hurts (e.g., election-week war room). Batch is correct for everyone until then.

## R7 — ART Adaptive Learning Loop (Phase 4)
Trajectory events (already streaming to opendesk.usage.events) train bandit-style policies: which template/timing/channel per person segment maximizes conversion, auto-allocating send volume toward winners with exploration caps.
**Trigger:** R1 copilot live (ART allocates within human-approved campaign structures, never autonomously).

## Explicit non-goals (revisit only with new evidence)
- Cross-tenant person-level graphs (permanent exclusion — RLS/NDPA/trust).
- Device fingerprinting (no device telemetry exists; would require mobile SDK wave first).
- Graph-native vector DB replacement (Ollama embeddings + FalkorDB suffice at tenant scale).
