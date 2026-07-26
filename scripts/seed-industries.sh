#!/usr/bin/env bash
# Seed the industry demo tenants (SPEC-CRM §C5). Uses the direct identity
# port (gateway /api/* requires a JWT). Each tenant is created with its
# industry pack id; the TenantOnboardingWorkflow then applies the pack
# (offerings, knowledge seed, terminology) via ApplyIndustryPack.
set -euo pipefail
IDENTITY="${IDENTITY:-http://localhost:7001}"

create_tenant() {
  local slug="$1" name="$2" industry="$3" tz="$4" currency="$5" locale="$6"
  echo "Seeding tenant $slug (industry: $industry)"
  curl -sf -X POST "$IDENTITY/v1/tenants" -H 'content-type: application/json' -d "{
    \"slug\":\"$slug\",\"name\":\"$name\",\"industry\":\"$industry\",
    \"timezone\":\"$tz\",\"currency\":\"$currency\",\"locale\":\"$locale\",\"plan\":\"pro\"
  }" | jq .
}

create_tenant acme-salon    "Acme Salon & Spa"      salon       "Europe/London" "GBP" "en-GB"
create_tenant acme-clinic   "Acme Health Clinic"    clinic      "Europe/Berlin" "EUR" "de-DE"
create_tenant acme-consult  "Acme Consulting Group" consultancy "America/New_York" "USD" "en-US"
create_tenant acme-support  "Acme Support Desk"     support-desk "UTC"          "USD" "en-US"
# Nigeria SME demo (NDPA profile: infra/privacy/ndpa-profile.env). Prices in
# the pack are kobo-denominated (NGN); timezone West Africa Time.
create_tenant acme-ng       "Acme Naija Ventures"   nigeria-sme "Africa/Lagos"  "NGN" "en-NG"
create_tenant acme-bank     "Acme Bank & Trust"     banking     "Africa/Lagos"  "NGN" "en-NG"
create_tenant acme-insure   "Acme Insurance Group"  insurance   "America/Chicago" "USD" "en-US"
create_tenant acme-shop     "Acme Online Store"     ecommerce   "Europe/London" "GBP" "en-GB"
# Wave: hospital, agribusiness and fashion-house demos (Africa/Lagos, NGN
# where local). healthcare/education/stock-brokerage packs have no fees.
create_tenant acme-health   "Acme Specialist Hospital" healthcare "Africa/Lagos" "NGN" "en-NG"
create_tenant acme-agro     "Acme Agro Cooperative"   agriculture "Africa/Lagos"  "NGN" "en-NG"
create_tenant acme-fashion  "Acme Fashion House"      fashion     "Africa/Lagos"  "NGN" "en-NG"
# Wave 6: developing-country vertical demos (SPEC-W6 Part B). All
# Africa/Lagos, NGN; pack prices are kobo-denominated.
create_tenant acme-mfb      "Acme Microfinance Cooperative" microfinance "Africa/Lagos" "NGN" "en-NG"
create_tenant acme-pharm    "Acme Pharmacy & Stores"        pharmacy     "Africa/Lagos" "NGN" "en-NG"
create_tenant acme-logistics "Acme Express Logistics"       logistics    "Africa/Lagos" "NGN" "en-NG"
# Wave 7: public-safety vertical demos (SPEC-W7 Part A). Non-emergency
# reporting only; the packs' hard rules route emergencies to the national
# emergency number first.
create_tenant city-civic      "City Civic Services (311)"     civic-services     "America/New_York" "USD" "en-US"
create_tenant community-watch "Community Watch Network"       neighborhood-watch "Europe/London"   "GBP" "en-GB"
# Wave 9: hospitality demo (SPEC-W9 Part D). Prices are kobo-denominated
# (NGN), per-night room rates with a first-night deposit; timezone WAT.
create_tenant acme-hotel   "Acme Grand Hotel"       hospitality "Africa/Lagos"  "NGN" "en-NG"

echo "Seed complete. Industry packs are applied asynchronously by the TenantOnboardingWorkflow."
