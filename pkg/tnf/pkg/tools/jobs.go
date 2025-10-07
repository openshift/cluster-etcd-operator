package tools

import (
	"fmt"
	"time"
)

// Define some constants used for Job timeouts.
// We currently have these jobs, started at the same time, but waiting for each other in the running job:
// auth job: starts immediately
// setup job: waits for auth jobs to complete
// after setup jobs: waits for setup job to complete
const (
	JobPollIntervall               = 15 * time.Second
	AuthJobCompletedTimeout        = 10 * time.Minute
	SetupJobCompletedTimeout       = 20 * time.Minute
	FencingJobCompletedTimeout     = 25 * time.Minute
	RestartEtcdJobCompletedTimeout = 10 * time.Minute
)

// JobType represent the different jobs we run, with some methods needed
// for running the jobs
type JobType int

const (
	JobTypeAuth JobType = iota
	JobTypeSetup
	JobTypeAfterSetup
	JobTypeFencing
	JobTypeRestartEtcd
)

func (t JobType) GetSubCommand() string {
	switch t {
	case JobTypeAuth:
		return "auth"
	case JobTypeSetup:
		return "setup"
	case JobTypeAfterSetup:
		return "after-setup"
	case JobTypeFencing:
		return "fencing"
	case JobTypeRestartEtcd:
		return "restart-etcd"
	default:
		return ""
	}
}

func (t JobType) GetJobName(nodeName *string) string {
	name := fmt.Sprintf("tnf-%s-job", t.GetSubCommand())
	if nodeName != nil {
		name = fmt.Sprintf("%s-%s", name, *nodeName)
	}
	return name
}

func (t JobType) GetNameLabelValue() string {
	return t.GetJobName(nil)
}
