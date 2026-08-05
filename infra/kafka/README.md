# infra/kafka — OpenDesk Kafka data plane (SPEC §4)

## Broker
- Image: `bitnami/kafka:3.7`, single-node **KRaft** (no ZooKeeper).
- Listener: `kafka:9092` (PLAINTEXT, dev only). Controller on internal `9093`.
- **Auto-create is OFF** (`KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE=false`) so topic
  names/partitions stay a deliberate contract, not an accident of a typo'd producer.

## Topics (created by the one-shot `kafka-topics` container)
`kafka/create-topics.sh` runs after the broker is healthy and creates, idempotently
(`--create --if-not-exists`), every topic from SPEC §4 with **6 partitions, rf 1**:

| Topic | Payload |
|---|---|
| `opendesk.booking.commands` | BookAppointment / RescheduleAppointment / CancelAppointment (key: bookingId) |
| `opendesk.booking.events` | BookingCreated/Confirmed/Rescheduled/Cancelled/NoShow (CloudEvents JSON) |
| `opendesk.conversation.transcripts` | ConversationTurn {conversationId, tenantId, role, text, ts, audioUrl?} |
| `opendesk.conversation.events` | SessionStarted/Ended, ToolInvoked |
| `opendesk.payments.commands` | ChargeDeposit, Refund, NoShowFee |
| `opendesk.payments.events` | PaymentPosted(ledgerRef) |
| `opendesk.identity.events` | TenantProvisioned, MemberInvited, RoleChanged |
| `opendesk.notifications.outbox` | SendReminder, SendConfirmation, SendPortalCode (Wave 5 #7: booking-service publishes the 6-digit portal login code; notification-worker consumes it via group `notification-outbox` and delivers it through the smtp/twilio bindings) |
| `opendesk.dlq` | dead letters |

All messages use the **CloudEvents 1.0** envelope:
`{specversion, id, source, type, subject, time, tenantid (ext), data}`.

## Operations
```bash
# list topics
docker exec kafka /opt/bitnami/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list
# re-run topic creation (idempotent)
docker compose -f infra/docker-compose.core.yml up kafka-topics
```

Consumers reach the broker via Dapr pubsub (`infra/dapr/components/pubsub.kafka.yaml`),
broker `kafka:9092`, `authType: none`; consumer groups are set per app at runtime.
