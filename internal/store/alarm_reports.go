package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// GetOccurrence returns one outstanding alarm. ErrOccurrenceGone if it has
// completed (returned to normal and acknowledged) or never existed.
func (s *Store) GetOccurrence(ctx context.Context, id int64) (Occurrence, error) {
	var o Occurrence
	err := scanOccurrence(s.pool.QueryRow(ctx, occurrenceSelect+` WHERE occurrence_id = $1`, id), &o)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrOccurrenceGone
	}
	return o, err
}

// HistoryEvent is one row of alarm_history with the alarm it belongs to.
type HistoryEvent struct {
	ID           int64
	OccurrenceID int64
	AlarmID      int64
	Event        string
	Prev, Next   string // '' = none
	Value        *float64
	TS           time.Time
	UserName     string // '' = automatic
	Details      string
	Reason       string
	Priority     string // at the time of the event
	Setpoint     *float64

	PLCName, PVName, Unit, AlarmType, Message string
	Digital                                   bool
	State0Text, State1Text                    string
}

const historySelect = `
	SELECT h.event_id, h.occurrence_id, h.alarm_id, h.event_type, COALESCE(h.previous_state,''), COALESCE(h.new_state,''),
	       h.pv_value, h.event_timestamp, h.user_name, h.details, h.reason, COALESCE(h.priority,''), h.setpoint,
	       plc.plc_name, pv.pv_name, pv.unit, m.alarm_type, m.alarm_message,
	       pv.register_address BETWEEN 10001 AND 19999, pv.state0_text, pv.state1_text
	FROM alarm_history h
	JOIN alarm_metadata m ON m.alarm_id = h.alarm_id
	JOIN pv_metadata pv   ON pv.pv_id = m.pv_id
	JOIN plc_metadata plc ON plc.plc_id = pv.plc_id`

func scanHistory(r pgx.CollectableRow) (HistoryEvent, error) {
	var h HistoryEvent
	err := r.Scan(&h.ID, &h.OccurrenceID, &h.AlarmID, &h.Event, &h.Prev, &h.Next, &h.Value, &h.TS, &h.UserName,
		&h.Details, &h.Reason, &h.Priority, &h.Setpoint, &h.PLCName, &h.PVName, &h.Unit, &h.AlarmType, &h.Message,
		&h.Digital, &h.State0Text, &h.State1Text)
	return h, err
}

// OccurrenceEvents returns the whole timeline of one occurrence, oldest first.
func (s *Store) OccurrenceEvents(ctx context.Context, occ int64) ([]HistoryEvent, error) {
	rows, err := s.pool.Query(ctx, historySelect+` WHERE h.occurrence_id = $1 ORDER BY h.event_id`, occ)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanHistory)
}

// HistoryFilter selects events for the alarm history log. Zero values mean "all".
type HistoryFilter struct {
	Since    time.Time
	Priority string // priority at the time of the event
	AlarmID  int64
	Event    string
	Limit    int
}

// AlarmEvents returns events newest first, plus how many match in total (so
// the page can say when the list is cut at Limit).
func (s *Store) AlarmEvents(ctx context.Context, f HistoryFilter) (events []HistoryEvent, total int, err error) {
	where := ` WHERE ($1::timestamptz IS NULL OR h.event_timestamp >= $1)
	             AND ($2 = '' OR h.priority = $2)
	             AND ($3 = 0 OR h.alarm_id = $3)
	             AND ($4 = '' OR h.event_type = $4)`
	var since *time.Time
	if !f.Since.IsZero() {
		since = &f.Since
	}
	args := []any{since, f.Priority, f.AlarmID, f.Event}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_history h`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, historySelect+where+` ORDER BY h.event_id DESC LIMIT $5`, append(args, f.Limit)...)
	if err != nil {
		return nil, 0, err
	}
	events, err = pgx.CollectRows(rows, scanHistory)
	return events, total, err
}

// FrequentAlarm is one line of the "most frequent alarms" list.
type FrequentAlarm struct {
	AlarmID     int64
	PLCName     string
	PVName      string
	AlarmType   string
	Message     string
	Activations int
}

// AlarmStats are basic monitoring figures for a period (ISA-18.2 alarm system
// monitoring). They are plain counts: judging them against targets is a matter
// of the site's alarm philosophy.
type AlarmStats struct {
	Activations        int
	ByPriority         map[string]int
	Acknowledgements   int
	AvgTimeToAckSec    *float64 // activation → acknowledgement, for occurrences acknowledged while active
	MaxTimeToAckSec    *float64
	PeakIn10Min        int // most activations in any fixed 10-minute slot
	Shelvings          int
	Top                []FrequentAlarm
	OutstandingNow     int
	StandingOver24h    int // outstanding and activated more than 24 h ago
	ShelvedNow         int
	OutsideResponseMax int // acknowledged later than the alarm's max response time (in the period)
}

// AlarmStatistics computes the figures for events since `since`.
func (s *Store) AlarmStatistics(ctx context.Context, since time.Time) (AlarmStats, error) {
	st := AlarmStats{ByPriority: map[string]int{}}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE event_type='ACTIVATED' AND previous_state IS NULL),
		       count(*) FILTER (WHERE event_type='ACKNOWLEDGED'),
		       count(*) FILTER (WHERE event_type='SHELVED')
		FROM alarm_history WHERE event_timestamp >= $1`, since).Scan(&st.Activations, &st.Acknowledgements, &st.Shelvings); err != nil {
		return st, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(priority,''), count(*) FROM alarm_history
		WHERE event_timestamp >= $1 AND event_type='ACTIVATED' AND previous_state IS NULL GROUP BY 1`, since)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return st, err
		}
		st.ByPriority[p] = n
	}
	rows.Close()

	// time to acknowledge: UNACK → ACKED, measured activation → acknowledgement
	if err := s.pool.QueryRow(ctx, `
		WITH ack AS (
		    SELECT a.event_timestamp - act.event_timestamp AS took, m.max_response_time_s
		    FROM alarm_history a
		    JOIN alarm_history act ON act.occurrence_id = a.occurrence_id AND act.event_type='ACTIVATED' AND act.previous_state IS NULL
		    JOIN alarm_metadata m ON m.alarm_id = a.alarm_id
		    WHERE a.event_type='ACKNOWLEDGED' AND a.previous_state='UNACK' AND a.event_timestamp >= $1)
		SELECT avg(extract(epoch from took)), max(extract(epoch from took)),
		       count(*) FILTER (WHERE max_response_time_s IS NOT NULL AND extract(epoch from took) > max_response_time_s)
		FROM ack`, since).Scan(&st.AvgTimeToAckSec, &st.MaxTimeToAckSec, &st.OutsideResponseMax); err != nil {
		return st, err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(max(n),0) FROM (
		    SELECT count(*) AS n FROM alarm_history
		    WHERE event_timestamp >= $1 AND event_type='ACTIVATED' AND previous_state IS NULL
		    GROUP BY floor(extract(epoch from event_timestamp) / 600)) x`, since).Scan(&st.PeakIn10Min); err != nil {
		return st, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT h.alarm_id, plc.plc_name, pv.pv_name, m.alarm_type, m.alarm_message, count(*) AS n
		FROM alarm_history h
		JOIN alarm_metadata m ON m.alarm_id = h.alarm_id
		JOIN pv_metadata pv ON pv.pv_id = m.pv_id
		JOIN plc_metadata plc ON plc.plc_id = pv.plc_id
		WHERE h.event_timestamp >= $1 AND h.event_type='ACTIVATED' AND h.previous_state IS NULL
		GROUP BY h.alarm_id, plc.plc_name, pv.pv_name, m.alarm_type, m.alarm_message
		ORDER BY n DESC, h.alarm_id LIMIT 5`, since)
	if err != nil {
		return st, err
	}
	st.Top, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (FrequentAlarm, error) {
		var f FrequentAlarm
		return f, r.Scan(&f.AlarmID, &f.PLCName, &f.PVName, &f.AlarmType, &f.Message, &f.Activations)
	})
	if err != nil {
		return st, err
	}

	return st, s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE activated_at < now() - interval '24 hours'),
		       count(*) FILTER (WHERE alarm_state = 'SHLVD')
		FROM active_alarms`).Scan(&st.OutstandingNow, &st.StandingOver24h, &st.ShelvedNow)
}
