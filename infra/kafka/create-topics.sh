#!/bin/bash
# create-topics.sh — one-shot topic init for the OpenDesk Kafka broker (SPEC §4).
# All topics: 6 partitions, replication-factor 1 (dev). Broker auto-create is OFF,
# so every topic the platform uses must be declared here.
set -euo pipefail

BOOTSTRAP="${BOOTSTRAP:-kafka:9092}"
KT=/opt/bitnami/kafka/bin/kafka-topics.sh

TOPICS=(
  opendesk.booking.commands        # BookAppointment / RescheduleAppointment / CancelAppointment (key: bookingId)
  opendesk.booking.events          # BookingCreated/Confirmed/Rescheduled/Cancelled/NoShow (CloudEvents JSON)
  opendesk.conversation.transcripts # ConversationTurn events
  opendesk.transcripts-raw        # raw telephony/edge transcripts (Fluvio mirror source)
  opendesk.conversation.events     # SessionStarted/Ended, ToolInvoked, EscalationRequested
  opendesk.conversation.enriched   # per-turn call-intelligence enrichment (sentiment/intent/entities)
  opendesk.conversation.quality    # CallQualityEnriched: SessionEnded quality + avg sentiment (Wave 5 #2)
  opendesk.payments.commands       # ChargeDeposit, Refund, NoShowFee
  opendesk.payments.events         # PaymentPosted(ledgerRef)
  opendesk.identity.events         # TenantProvisioned, MemberInvited, RoleChanged
  opendesk.crm.events              # CRM webhook intake + priority flags (SPEC-CRM §B)
  opendesk.notifications.outbox    # SendReminder, SendConfirmation
  opendesk.privacy.events          # PrivacyEraseRequested tombstones (GDPR, SPEC-W3 §2)
  opendesk.usage.events            # UsageRecord metering events (Wave 5 #9: bookings, call-minutes, tokens)
  opendesk.incidents               # IDPCreated Incident Data Packets (SPEC-W11: conversation-service emits, booking-service consumes)
  opendesk.consent.erasure.v1      # consent ErasureRequested tombstones (SPEC-W12 §4: identity-service emits)
  opendesk.kyc.resolved.v1         # KYC Resolved results (SPEC-W12 §5: kyc-service emits)
  cac.events                       # CAC program event stream (SPEC-W12 §7)
  opendesk.dlq                     # dead letters
  # SPEC-W18 (additive): app lifecycle events (identity-service emits;
  # portal/app-catalog consumers) — was missing from the declarations.
  opendesk.apps.lifecycle.v1       # AppProvisioned/Enabled/Disabled/Suspended (SPEC-W18)
  # SPEC-W19 (additive): enterprise app lifecycle event streams
  # (booking-service emits via the transactional outbox).
  opendesk.helpdesk.events.v1      # helpdesk TicketEvent: ticket_created/ticket_resolved (SPEC-W19 Agent A)
  opendesk.fsm.events.v1           # field-service WorkOrderAssigned/WorkOrderCompleted (SPEC-W19 Agent B)
  opendesk.loyalty.events.v1       # loyalty PointsIssued/PointsRedeemed (SPEC-W19 Agent C)
  opendesk.studio.events.v1        # campaign-studio journey lifecycle events (SPEC-W19 Agent D)
  # SPEC-W20 (additive): batch-2 enterprise app lifecycle event streams
  # (booking-service emits via the transactional outbox).
  opendesk.crm.events.v1           # crm-360 note/pin/tag changes (SPEC-W20 Agent A)
  opendesk.surveys.events.v1       # surveys sent/answered (SPEC-W20 Agent B)
  opendesk.lending.events.v1       # lending application decided/disbursed/repaid + disbursement intent (SPEC-W20 Agent C)
  opendesk.workforce.events.v1     # workforce shift assigned/leave decided (SPEC-W20 Agent D)
  # SPEC-W21 (additive): social-publisher lifecycle events
  # (booking-service emits via the transactional outbox).
  opendesk.social.events.v1        # social-publisher PostPublished/AdLaunched/AdRejected (SPEC-W21 Agent B)
  # CAC seed report pattern (scripts/seeds/_lib.py emit_seed_report):
  # topic is f"cac.seed.report.{table}.v1" — the seed scripts pass their
  # schema-qualified TABLE constant, so the declared names below match the
  # emitted topics EXACTLY (verified against scripts/seeds/seed_*.py).
  cac.seed.report.cac.lgas.v1              # seed_lgas.py (TABLE=cac.lgas)
  cac.seed.report.cac.wards.v1             # seed_wards.py (TABLE=cac.wards)
  cac.seed.report.cac.channels.v1          # seed_channels.py (TABLE=cac.channels)
  cac.seed.report.cac.channel_unit_costs.v1 # seed_channel_costs.py (TABLE=cac.channel_unit_costs)
  cac.seed.report.cac.agents.v1            # seed_agents.py (TABLE=cac.agents)
  cac.seed.report.cac.customers.v1         # seed_customers.py (TABLE=cac.customers)
  cac.seed.report.cac.fx_series.v1         # seed_fx.py (TABLE=cac.fx_series)
  cac.seed.report.locale_coverage.v1       # seed_locale.py (TABLE=locale_coverage)
  cac.seed.report.cac.events.v1            # seed_events.py (reports with table=cac.events)
  # SPEC-W28 (additive): tenant knowledge graph streams.
  opendesk.graph.enrichment.v1     # gold→graph enrichment rows (spark graph_enrichment.py → graph-sync)
  opendesk.graph.erasure.done.v1   # graph erasure audit events (graph-sync emits)
  # SPEC-W30 (additive): fraud & trust intelligence.
  opendesk.fraud.alerts.v1         # fraud-engine AlertRaised + graph-service AlertResolved audit events
  # SPEC-W32 (additive): civic reporting.
  opendesk.civic.events.v1         # civic case lifecycle (booking-service → notify/graph/fraud/analytics)
  # SPEC-W33 (additive): ML platform ops alerts (model-registry emits drift
  # sweep PSI breaches + nightly training-gate failures; SPEC-W33 §4 C2/C5).
  ops.alerts                       # model_drift / training_gate_failed / training_tick_error alerts
  # W39 INFRA-004 (additive): previously unprovisioned topics — broker
  # auto-create is OFF, so producers/consumers of these failed silently.
  opendesk.billing.events          # billing-engine BILLING_EVENTS_TOPIC default: com.opendesk.billing.* CloudEvents (InvoicePaid etc.)
  opendesk.cac.events              # prefixed CAC stream (fraud-engine FRAUD_KAFKA_TOPICS default consumes it; producers migrate from cac.events)
  opendesk.conversation.captures   # conversation-service CAPTURE_TOPIC default (CaptureExtracted, SPEC-W38 F3); crm-sync CAPTURES_TOPIC consumes
  opendesk.ops.alerts              # notification-worker OPS_ALERTS_TOPIC default (prefixed ops alerts stream)
)

echo "[kafka-topics] waiting for broker at ${BOOTSTRAP}..."
until /opt/bitnami/kafka/bin/kafka-broker-api-versions.sh --bootstrap-server "${BOOTSTRAP}" >/dev/null 2>&1; do
  sleep 2
done

for t in "${TOPICS[@]}"; do
  echo "[kafka-topics] creating ${t} (partitions=6 rf=1)"
  "${KT}" --bootstrap-server "${BOOTSTRAP}" \
    --create --if-not-exists \
    --topic "${t}" \
    --partitions 6 \
    --replication-factor 1
done

echo "[kafka-topics] done. Current topics:"
"${KT}" --bootstrap-server "${BOOTSTRAP}" --list
