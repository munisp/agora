//! Minimal Dapr HTTP pubsub helper (SPEC §3/§9): publishes CloudEvents to the
//! `pubsub-kafka` component which lands them on the Kafka topic
//! `opendesk.payments.events`.

use serde::Serialize;

use crate::events::CloudEvent;

#[derive(Debug, Clone)]
pub struct DaprOutbox {
    http: reqwest::Client,
    base_url: String,
    pubsub: String,
    topic: String,
}

impl DaprOutbox {
    pub fn new(base_url: String, pubsub: String, topic: String) -> Self {
        Self {
            // RS-006: shared client with 5s connect / 30s overall timeouts.
            http: crate::http_client(),
            base_url,
            pubsub,
            topic,
        }
    }

    /// POST /v1.0/publish/{pubsub}/{topic} with a CloudEvents JSON payload.
    pub async fn publish<T: Serialize>(&self, event: &CloudEvent<T>) -> Result<(), reqwest::Error> {
        let url = format!(
            "{}/v1.0/publish/{}/{}",
            self.base_url, self.pubsub, self.topic
        );
        self.http
            .post(url)
            .header("content-type", "application/cloudevents+json")
            .json(event)
            .send()
            .await?
            .error_for_status()?;
        Ok(())
    }
}
