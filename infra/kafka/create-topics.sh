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
  opendesk.dlq                     # dead letters
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
