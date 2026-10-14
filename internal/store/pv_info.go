package store

import (
	"context"
	"strconv"
	"time"
)

// PVInfo is the stored metadata of a PV that the live values do not carry:
// when it was created / changed, whether it is acquired, and how its PLC is
// reached. Read when a PV detail is opened.
type PVInfo struct {
	PVID           int
	Enabled        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	PLCID          int
	PLCName        string
	PLCEnabled     bool
	PLCDescription string
	PLCAddress     string // host:port
	UnitID         int
	ScanIntervalMS int
	TimeoutMS      int
}

// GetPVInfo returns the extra metadata of one PV. pgx.ErrNoRows if it does not exist.
func (s *Store) GetPVInfo(ctx context.Context, pvID int) (PVInfo, error) {
	var i PVInfo
	var host string
	var port int
	err := s.pool.QueryRow(ctx, `
		SELECT pv.pv_id, pv.enabled, pv.created_at, pv.updated_at,
		       p.plc_id, p.plc_name, p.enabled, p.description, host(p.ip_address), p.port, p.unit_id,
		       p.scan_interval_ms, p.timeout_ms
		FROM pv_metadata pv JOIN plc_metadata p ON p.plc_id = pv.plc_id
		WHERE pv.pv_id = $1`, pvID).
		Scan(&i.PVID, &i.Enabled, &i.CreatedAt, &i.UpdatedAt,
			&i.PLCID, &i.PLCName, &i.PLCEnabled, &i.PLCDescription, &host, &port, &i.UnitID,
			&i.ScanIntervalMS, &i.TimeoutMS)
	i.PLCAddress = host + ":" + strconv.Itoa(port)
	return i, err
}
