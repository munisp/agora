package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WaitlistEntry is a customer waiting for an earlier slot.
type WaitlistEntry struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	CustomerID   uuid.UUID `json:"customer_id"`
	TeamMemberID uuid.UUID `json:"team_member_id"`
	ServiceID    uuid.UUID `json:"service_id"`
	LocationID   uuid.UUID `json:"location_id"`
	Earliest     time.Time `json:"earliest"`
	Latest       time.Time `json:"latest"`
	Status       string    `json:"status"`
	Priority     int       `json:"priority"`
	CreatedAt    time.Time `json:"created_at"`
}

// WaitlistAdd places a customer on the waitlist.
func (s *Store) WaitlistAdd(ctx context.Context, tenantID uuid.UUID, e *WaitlistEntry) error {
	e.ID = uuid.New()
	e.TenantID = tenantID
	e.Status = "waiting"
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO booking_waitlist
		    (id, tenant_id, customer_id, team_member_id, service_id, location_id,
		     earliest_at, latest_at, status, priority)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,'waiting',$8,now()) RETURNING created_at`,
			e.ID, tenantID, e.CustomerID, e.TeamMemberID, e.ServiceID, e.LocationID,
			e.Earliest, e.Latest, e.Priority).Scan(&e.CreatedAt)
	})
}

// AvailabilityRule describes when a team member can be booked.
type AvailabilityRule struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	TeamMemberID uuid.UUID `json:"team_member_id"`
	Weekday      int       `json:"weekday"`
	StartMinute  int       `json:"start_minute"`
	EndMinute    int       `json:"end_minute"`
	EffectiveFrom time.Time `json:"effective_from"`
	EffectiveTo   time.Time `json:"effective_to"`
}

// AvailabilitySet replaces the weekly rules for a team member.
func (s *Store) AvailabilitySet(ctx context.Context, tenantID uuid.UUID, memberID uuid.UUID, rules []AvailabilityRule) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM availability_rules WHERE tenant_id=$1 AND team_member_id=$2`, tenantID, memberID); err != nil {
			return err
		}
		for _, r := range rules {
			id := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO availability_rules
			    (id, tenant_id, team_member_id, weekday, start_minute, end_minute, effective_from, effective_to)
			    VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				id, tenantID, memberID, r.Weekday, r.StartMinute, r.EndMinute, r.EffectiveFrom, r.EffectiveTo); err != nil {
				return err
			}
		}
		return nil
	})
}

// TimeOff is a booked-out interval for a team member.
type TimeOff struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	TeamMemberID uuid.UUID `json:"team_member_id"`
	StartsAt     time.Time `json:"starts_at"`
	EndsAt       time.Time `json:"ends_at"`
	Reason       string    `json:"reason"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TimeOffAdd records a time-off block.
func (s *Store) TimeOffAdd(ctx context.Context, tenantID uuid.UUID, t *TimeOff) error {
	t.ID = uuid.New()
	t.TenantID = tenantID
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO team_time_off
		    (id, tenant_id, team_member_id, starts_at, ends_at, reason)
		    VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at, updated_at`,
			t.ID, tenantID, t.TeamMemberID, t.StartsAt, t.EndsAt, t.Reason).Scan(&t.CreatedAt, &t.UpdatedAt)
	})
}

// TimeOffList returns time-off blocks for a member overlapping a window.
func (s *Store) TimeOffList(ctx context.Context, tenantID, memberID uuid.UUID, from, to time.Time) ([]TimeOff, error) {
	var out []TimeOff
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, team_member_id, starts_at, ends_at, reason, created_at, updated_at
			  FROM team_time_off
			 WHERE tenant_id=$1 AND team_member_id=$2
			   AND starts_at < $4 AND ends_at > $3
			 ORDER BY starts_at`, tenantID, memberID, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t TimeOff
			if err := rows.Scan(&t.ID, &t.TenantID, &t.TeamMemberID, &t.StartsAt, &t.EndsAt, &t.Reason, &t.CreatedAt, &t.UpdatedAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// AvailabilityList returns the weekly rules for a team member.
func (s *Store) AvailabilityList(ctx context.Context, tenantID, memberID uuid.UUID) ([]AvailabilityRule, error) {
	var out []AvailabilityRule
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, team_member_id, weekday, start_minute, end_minute, effective_from, effective_to
			  FROM availability_rules WHERE tenant_id=$1 AND team_member_id=$2
			 ORDER BY weekday, start_minute`, tenantID, memberID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r AvailabilityRule
			if err := rows.Scan(&r.ID, &r.TenantID, &r.TeamMemberID, &r.Weekday, &r.StartMinute, &r.EndMinute, &r.EffectiveFrom, &r.EffectiveTo); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// WaitlistList returns waiting entries for a member whose window overlaps.
func (s *Store) WaitlistList(ctx context.Context, tenantID, memberID uuid.UUID, from, to time.Time) ([]WaitlistEntry, error) {
	var out []WaitlistEntry
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, customer_id, team_member_id, service_id, location_id,
			       earliest_at, latest_at, status, priority, created_at
			  FROM booking_waitlist
			 WHERE tenant_id=$1 AND team_member_id=$2 AND status NOT IN ('cancelled')
			   AND starts_at < $4 AND ends_at > $3
toto`, tenantID, memberID, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e WaitlistEntry
			if err := rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.TeamMemberID, &e.ServiceID, &e.LocationID,
				&e.Earliest, &e.Latest, &e.Status, &e.Priority, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ErrWaitlistNotFound is returned when no entry matches.
var ErrWaitlistNotFound = errors.New("waitlist entry not found")

// WaitlistCancel marks an entry cancelled.
func (s *Store) WaitlistCancel(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE booking_waitlist SET status='cancelled'
			 WHERE tenant_id=$1 AND id=$2 AND status='waiting'`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrWaitlistNotFound
		}
		return nil
	})
}

// WaitlistPromote converts a waiting entry into a booked appointment.
func (s *Store) WaitlistPromote(ctx context.Context, tenantID, id uuid.UUID, startsAt, endsAt time.Time) (uuid.UUID, error) {
	var apptID uuid.UUID
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var e WaitlistEntry
		err := tx.QueryRow(ctx, `SELECT id, tenant_id, customer_id, team_member_id, service_id, location_id,
			       earliest_at, latest_at, status, priority, created_at
			  FROM booking_waitlist WHERE tenant_id=$1 AND id=$2 AND status='waiting' FOR UPDATE`,
			tenantID, id).Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.TeamMemberID, &e.ServiceID, &e.LocationID,
			&e.Earliest, &e.Latest, &e.Status, &e.Priority, &e.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWaitlistNotFound
		}
		if err != nil {
			return err
		}
		apptID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO appointments
		    (id, tenant_id, customer_id, team_member_id, service_id, location_id,
		     starts_at, ends_at, status, created_at, updated_at)
		             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now(),now())
		             RETURNING created_at, updated_at`,
			apptID, tenantID, e.CustomerID, e.TeamMemberID, e.ServiceID, e.LocationID,
			startsAt, endsAt, "booked", 0); err != nil {
			return fmt.Errorf("promote insert: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE booking_waitlist SET status='promoted' WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		return err
	})
	return apptID, err
}
