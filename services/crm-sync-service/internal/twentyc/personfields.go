package twentyc

import (
	"context"
	"strings"
)

// PersonFields (2026-11-23) maps facts Agora knows about a contact onto a business's own CRM fields (TWENTY_ROUTES
// "person_fields"). Every entry is optional; a fact with no field, or whose value has no mapping, is skipped.
//
//	{"advisor": "assignedAdvisor",
//	 "first_channel": {"field": "firstContactChannel", "values": {"voice": "PHONE", "web": "BOOKING_PAGE"}},
//	 "language": {"field": "preferredLanguage", "values": {"en": "ENGLISH", "yo": "YORUBA"}},
//	 "offering_status": {"field": "rsaStatus", "match": [["transfer", "TRANSFER"], ["registration", "REGISTERING"]]},
//	 "site": "leadway-pensure"}
//
// Advisor and language follow the latest booking / call; first channel and offering status are only filled while
// the field is empty (staff edits in the CRM win).
type PersonFields struct {
	Advisor        string   `json:"advisor"`
	FirstChannel   *Mapped  `json:"first_channel"`
	Language       *Mapped  `json:"language"`
	OfferingStatus *Matched `json:"offering_status"`
	Site           string   `json:"site"` // booking site slug whose team list names the advisor
}

// Mapped is a field whose value is looked up from a fixed table.
type Mapped struct {
	Field  string            `json:"field"`
	Values map[string]string `json:"values"`
}

// Matched is a field set from the first [substring, value] pair found in a text (case-insensitive).
type Matched struct {
	Field string      `json:"field"`
	Match [][2]string `json:"match"`
}

func (m *Mapped) value(key string) (string, string) {
	if m == nil || m.Field == "" {
		return "", ""
	}
	return m.Field, m.Values[strings.ToLower(strings.TrimSpace(key))]
}

func (m *Matched) value(text string) (string, string) {
	if m == nil || m.Field == "" {
		return "", ""
	}
	t := strings.ToLower(text)
	for _, pair := range m.Match {
		if pair[0] != "" && strings.Contains(t, strings.ToLower(pair[0])) {
			return m.Field, pair[1]
		}
	}
	return m.Field, ""
}

// PersonFacts are what Agora knows at one moment (empty = unknown).
type PersonFacts struct {
	Advisor  string // advisor's display name
	Channel  string // booking source / call channel ("voice", "web", "phone", "whatsapp", ...)
	Language string // call language code ("en", "yo", ...)
	Offering string // offering name of a booking
}

// Patches splits the facts into fields that always follow the latest value and fields filled only while empty.
func (f *PersonFields) Patches(facts PersonFacts) (latest, ifEmpty map[string]any) {
	latest, ifEmpty = map[string]any{}, map[string]any{}
	if f == nil {
		return
	}
	if f.Advisor != "" && strings.TrimSpace(facts.Advisor) != "" {
		latest[f.Advisor] = strings.TrimSpace(facts.Advisor)
	}
	if field, v := f.Language.value(facts.Language); field != "" && v != "" {
		latest[field] = v
	}
	if field, v := f.FirstChannel.value(facts.Channel); field != "" && v != "" {
		ifEmpty[field] = v
	}
	if field, v := f.OfferingStatus.value(facts.Offering); field != "" && v != "" {
		ifEmpty[field] = v
	}
	return
}

// GetPersonFields reads a Person's current field values (custom fields included).
func (c *Client) GetPersonFields(ctx context.Context, id string) (map[string]any, error) {
	out := map[string]any{}
	err := c.getByID(ctx, "people", id, &out)
	return out, err
}

// PatchPerson writes the given fields on a Person.
func (c *Client) PatchPerson(ctx context.Context, id string, fields map[string]any) error {
	return c.patch(ctx, "people", id, fields)
}
