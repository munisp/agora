// Package twentyc is a minimal Twenty CRM REST client (net/http only):
// Bearer auth, JSON filter encoding, token-bucket rate limiting
// (TWENTY_RATE_PER_MIN, default 90/min) and retry with exponential backoff
// on 429/5xx (SPEC-CRM §B).
//
// Batch awareness: Twenty's /rest/batch/* endpoints accept up to 60
// operations per call. This client uses single-record endpoints only; if
// sync volume ever demands batching, group mutations in chunks of <=60.
package twentyc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxAttempts per Twenty call (initial try + retries on 429/5xx).
const maxAttempts = 4

// ErrNotFound is returned when a filtered list query yields no records.
var ErrNotFound = errors.New("twenty: record not found")

// Observer records one logical Twenty API call's total duration (including
// any retries), labelled by HTTP method and path class. May be nil.
type Observer func(method, pathClass string, d time.Duration)

// Client talks to the Twenty REST API.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
	limiter *tokenBucket
	// Observe is invoked after every logical API call (success or failure)
	// with its total duration; wired to the metrics registry in main.
	Observe Observer
}

// New builds a Client. ratePerMin <= 0 disables throttling.
func New(baseURL, apiKey string, ratePerMin int) *Client {
	c := &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		hc:      &http.Client{Timeout: 20 * time.Second},
	}
	if ratePerMin > 0 {
		c.limiter = newTokenBucket(ratePerMin)
	}
	return c
}

// tokenBucket is a simple mutex-guarded token bucket refilled once per minute.
type tokenBucket struct {
	mu       sync.Mutex
	capacity int
	tokens   int
	refillAt time.Time
	nowFn    func() time.Time
}

func newTokenBucket(perMin int) *tokenBucket {
	return &tokenBucket{capacity: perMin, tokens: perMin, refillAt: time.Now().Add(time.Minute), nowFn: time.Now}
}

// wait blocks until a token is available or ctx is done.
func (b *tokenBucket) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := b.nowFn()
		if !now.Before(b.refillAt) {
			b.tokens = b.capacity
			b.refillAt = now.Add(time.Minute)
		}
		if b.tokens > 0 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		delay := b.refillAt.Sub(now)
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// apiError carries a non-2xx Twenty response.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("twenty API status %d: %s", e.Status, e.Body) }

// pathClassOf maps a request path to a low-cardinality class for metrics
// labels: "/rest/people" and "/rest/people/{id}" both -> "people".
func pathClassOf(path string) string {
	p := strings.TrimPrefix(path, "/rest/")
	seg, _, _ := strings.Cut(p, "/")
	if seg == "" {
		return "root"
	}
	return seg
}

// retryable reports whether the status is worth retrying (429/5xx).
func retryable(status int) bool { return status == http.StatusTooManyRequests || status >= 500 }

// do performs one HTTP call with rate limiting and retry/backoff.
// out may be nil. Returns the decoded body wrapper {"data": ...}.
func (c *Client) do(ctx context.Context, method, path string, query string, body any, out any) error {
	start := time.Now()
	if c.Observe != nil {
		defer func() { c.Observe(method, pathClassOf(path), time.Since(start)) }()
	}
	var payload []byte
	var err error
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if c.limiter != nil {
			if err := c.limiter.wait(ctx); err != nil {
				return err
			}
		}
		url := c.baseURL + path
		if query != "" {
			url += "?" + query
		}
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
		} else {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if out != nil && len(b) > 0 {
					if err := json.Unmarshal(b, out); err != nil {
						return fmt.Errorf("decode response: %w", err)
					}
				}
				return nil
			}
			lastErr = &apiError{Status: resp.StatusCode, Body: string(b)}
			if !retryable(resp.StatusCode) {
				return lastErr // 4xx (except 429) will not heal with retries
			}
		}
		if attempt == maxAttempts {
			break
		}
		delay := backoff(attempt, retryAfter(lastErr))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}

func backoff(attempt int, retryAfterHint time.Duration) time.Duration {
	if retryAfterHint > 0 {
		return retryAfterHint
	}
	d := time.Duration(attempt*attempt) * 500 * time.Millisecond
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

func retryAfter(err error) time.Duration {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests {
		if n, perr := strconv.Atoi(ae.Body); perr == nil && n > 0 && n < 120 {
			return time.Duration(n) * time.Second
		}
		return 2 * time.Second
	}
	return 0
}

// envelope is Twenty's REST response wrapper: {"data": {<op>: <record|[records]>}, ...}.
type envelope struct {
	Data map[string]json.RawMessage `json:"data"`
}

// firstRecord extracts the first object of the first key in data (list or single).
func (e envelope) firstRecord() (json.RawMessage, error) {
	for _, raw := range e.Data {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil {
			if len(arr) == 0 {
				return nil, ErrNotFound
			}
			return arr[0], nil
		}
		var obj json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil && len(obj) > 0 && string(obj) != "null" {
			return obj, nil
		}
	}
	return nil, ErrNotFound
}

// Record is the minimal shape we read back from Twenty (id + passthrough).
type Record struct {
	ID string `json:"id"`
}

// listFirst returns the first record matching filter for a Twenty object
// (people|companies|tasks|notes), or ErrNotFound.
func (c *Client) listFirst(ctx context.Context, object, filter string) (Record, error) {
	var env envelope
	if err := c.do(ctx, http.MethodGet, "/rest/"+object, Query(filter, 1).Encode(), nil, &env); err != nil {
		return Record{}, err
	}
	raw, err := env.firstRecord()
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("decode %s record: %w", object, err)
	}
	if rec.ID == "" {
		return Record{}, ErrNotFound
	}
	return rec, nil
}

// create POSTs a record and returns its id.
func (c *Client) create(ctx context.Context, object string, body any) (string, error) {
	var env envelope
	if err := c.do(ctx, http.MethodPost, "/rest/"+object, "", body, &env); err != nil {
		return "", err
	}
	raw, err := env.firstRecord()
	if err != nil {
		return "", err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil || rec.ID == "" {
		return "", fmt.Errorf("create %s: no id in response", object)
	}
	return rec.ID, nil
}

// patch PATCHes a record by id.
func (c *Client) patch(ctx context.Context, object, id string, body any) error {
	return c.do(ctx, http.MethodPatch, "/rest/"+object+"/"+id, "", body, nil)
}

// getByID fetches a single record (GET /rest/{object}/{id}) and decodes it
// into out (the record object inside the response envelope).
func (c *Client) getByID(ctx context.Context, object, id string, out any) error {
	var env envelope
	if err := c.do(ctx, http.MethodGet, "/rest/"+object+"/"+id, "", nil, &env); err != nil {
		return err
	}
	raw, err := env.firstRecord()
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s/%s record: %w", object, id, err)
	}
	return nil
}

// ---------------- People ----------------

// FindPerson locates a person by email first, then phone (SPEC-CRM §B).
func (c *Client) FindPerson(ctx context.Context, email, phone string) (Record, error) {
	if email != "" {
		rec, err := c.listFirst(ctx, "people", PersonEmailFilter(email))
		if err == nil {
			return rec, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Record{}, err
		}
	}
	if phone != "" {
		return c.listFirst(ctx, "people", PersonPhoneFilter(phone))
	}
	return Record{}, ErrNotFound
}

// CreatePerson creates a Person record.
func (c *Client) CreatePerson(ctx context.Context, p PersonUpsert) (string, error) {
	return c.create(ctx, "people", p)
}

// UpdatePerson patches a Person record.
func (c *Client) UpdatePerson(ctx context.Context, id string, p PersonUpsert) error {
	return c.patch(ctx, "people", id, p)
}

// GetPerson fetches a full Person record by id (reverse sync: the webhook
// payload carries the id; the record is re-fetched for the current state).
func (c *Client) GetPerson(ctx context.Context, id string) (PersonRecord, error) {
	var p PersonRecord
	if err := c.getByID(ctx, "people", id, &p); err != nil {
		return p, err
	}
	return p, nil
}

// DeletePerson removes a Person record (GDPR right-to-erasure, SPEC-W3 §2).
// A 404 is treated as success — the record is already gone.
func (c *Client) DeletePerson(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/rest/people/"+id, "", nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// UpsertPerson find-then-create/update by email or phone.
func (c *Client) UpsertPerson(ctx context.Context, name, email, phone string) (string, error) {
	body := PersonFromContact(name, email, phone)
	rec, err := c.FindPerson(ctx, email, phone)
	if errors.Is(err, ErrNotFound) {
		return c.CreatePerson(ctx, body)
	}
	if err != nil {
		return "", err
	}
	if err := c.UpdatePerson(ctx, rec.ID, body); err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ---------------- Companies ----------------

// UpsertCompany find-by-name-then-create/update a Company (tenant mapping).
func (c *Client) UpsertCompany(ctx context.Context, name, slug string) (string, error) {
	body := CompanyUpsert{Name: name, DomainName: Links{PrimaryLinkURL: "https://" + TenantDomain(slug)}}
	rec, err := c.listFirst(ctx, "companies", CompanyNameFilter(name))
	if errors.Is(err, ErrNotFound) {
		return c.create(ctx, "companies", body)
	}
	if err != nil {
		return "", err
	}
	if err := c.patch(ctx, "companies", rec.ID, body); err != nil {
		return "", err
	}
	return rec.ID, nil
}

// GetCompany fetches a Company record by id (reverse sync: tenant resolution
// via the company domainName "<slug>.opendesk.local").
func (c *Client) GetCompany(ctx context.Context, id string) (CompanyRecord, error) {
	var comp CompanyRecord
	if err := c.getByID(ctx, "companies", id, &comp); err != nil {
		return comp, err
	}
	return comp, nil
}

// ---------------- Tasks ----------------

// CreateTask creates a Task and best-effort links it to a person via
// /rest/taskTargets. Link failures are logged by the caller, not fatal.
func (c *Client) CreateTask(ctx context.Context, t TaskCreate, personID string) (string, error) {
	id, err := c.create(ctx, "tasks", t)
	if err != nil {
		return "", err
	}
	if personID != "" {
		if err := c.createTaskTarget(ctx, id, personID); err != nil {
			return id, fmt.Errorf("task created (%s) but person link failed: %w", id, err)
		}
	}
	return id, nil
}

func (c *Client) createTaskTarget(ctx context.Context, taskID, personID string) error {
	_, err := c.create(ctx, "taskTargets", map[string]string{"taskId": taskID, "personId": personID})
	return err
}

// PatchTask patches arbitrary task fields (e.g. {"status":"DONE"}, {"dueAt":...}).
func (c *Client) PatchTask(ctx context.Context, id string, fields map[string]any) error {
	return c.patch(ctx, "tasks", id, fields)
}

// GetTask fetches a Task record by id (reverse sync: task.updated payloads
// that omit the new status).
func (c *Client) GetTask(ctx context.Context, id string) (TaskRecord, error) {
	var t TaskRecord
	if err := c.getByID(ctx, "tasks", id, &t); err != nil {
		return t, err
	}
	return t, nil
}

// ---------------- Notes ----------------

// CreateNote creates a Note and links it to a person via /rest/noteTargets.
func (c *Client) CreateNote(ctx context.Context, title, body, personID string) (string, error) {
	id, err := c.create(ctx, "notes", map[string]any{"title": title, "bodyV2": RichText{Markdown: body}})
	if err != nil {
		return "", err
	}
	if personID != "" {
		if _, err := c.create(ctx, "noteTargets", map[string]string{"noteId": id, "personId": personID}); err != nil {
			return id, fmt.Errorf("note created (%s) but person link failed: %w", id, err)
		}
	}
	return id, nil
}

// UpdateNote patches a Note's title/body (used by the Wave 5 #2 merge path
// when a sentiment-enriched CallQualityEnriched event arrives after the
// plain SessionEnded fallback already created the call-summary note).
func (c *Client) UpdateNote(ctx context.Context, id, title, body string) error {
	return c.patch(ctx, "notes", id, map[string]any{"title": title, "bodyV2": RichText{Markdown: body}})
}
