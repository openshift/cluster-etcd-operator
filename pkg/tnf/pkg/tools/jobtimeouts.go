package tools

import "time"

// Define some constants used for Job timeouts.
// We currently have these jobs, started at the same time, but waiting for each other in the running job:
// auth job: starts immediately
// setup job: waits for auth jobs to complete
// after setup jobs: waits for setup job to complete

const (
	JobPollIntervall           = 15 * time.Second
	AuthJobCompletedTimeout    = 10 * time.Minute
	SetupJobCompletedTimeout   = 20 * time.Minute
	FencingJobCompletedTimeout = 25 * time.Minute
)
