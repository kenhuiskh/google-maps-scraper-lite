package gmaps

import (
	"context"
	"time"
)

type JobProcess struct {
	JobID     string
	PID       int
	PGID      int
	StartedAt time.Time
}

// RecordJobProcess stores (or replaces) the process-group registry row for a
// job after its subprocess starts.
func (s *JobStore) RecordJobProcess(ctx context.Context, jobID string, pid, pgid int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_processes (job_id, pid, pgid, started_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET pid = excluded.pid, pgid = excluded.pgid, started_at = excluded.started_at`,
		jobID, pid, pgid, time.Now().UTC())
	return err
}

// DeleteJobProcess removes a job's registry row after a clean exit.
func (s *JobStore) DeleteJobProcess(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM job_processes WHERE job_id = ?`, jobID)
	return err
}

// ListJobProcesses returns all recorded process groups. After clean exits only
// leaked groups remain.
func (s *JobStore) ListJobProcesses(ctx context.Context) ([]JobProcess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, pid, pgid, started_at FROM job_processes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var procs []JobProcess
	for rows.Next() {
		var p JobProcess
		if err := rows.Scan(&p.JobID, &p.PID, &p.PGID, &p.StartedAt); err != nil {
			return nil, err
		}
		procs = append(procs, p)
	}
	return procs, rows.Err()
}
