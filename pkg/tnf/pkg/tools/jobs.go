package tools

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"
)

// Define some constants used for Job timeouts.
// We currently have these jobs, started at the same time, but waiting for each other in the running job:
// auth job: starts immediately
// setup job: waits for auth jobs to complete
// after setup jobs: waits for setup job to complete
const (
	JobPollInterval               = 15 * time.Second
	AuthJobCompletedTimeout       = 10 * time.Minute
	SetupJobCompletedTimeout      = 20 * time.Minute
	AfterSetupJobCompletedTimeout = 5 * time.Minute
	AllCompletedTimeout           = 30 * time.Minute
	FencingJobCompletedTimeout    = 25 * time.Minute
)

// JobType represent the different jobs we run, with some methods needed
// for running the jobs
type JobType int

const (
	JobTypeAuth JobType = iota
	JobTypeSetup
	JobTypeAfterSetup
	JobTypeFencing
	JobTypeUpdateSetup
)

const (
	MaxK8sResourceNameLength = 63
	PodSuffixBuffer          = 6
)

// Pre-compile regex once at startup for performance
var nodeRolePattern = regexp.MustCompile(`^([a-z0-9]+-\d+)`)

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
	case JobTypeUpdateSetup:
		return "update-setup"
	default:
		return ""
	}
}

func (t JobType) GetJobName(nodeName *string) string {
	name := fmt.Sprintf("tnf-%s-job", t.GetSubCommand())
	if nodeName != nil {
		maxSuffixLen := MaxK8sResourceNameLength - len(name) - PodSuffixBuffer - 1
		formattedName := shortenNodeName(*nodeName, maxSuffixLen)
		name = fmt.Sprintf("%s-%s", name, formattedName)
	}
	return name
}

func (t JobType) GetNameLabelValue() string {
	return t.GetJobName(nil)
}

func shortenNodeName(nodeName string, maxLength int) string {
	// generate hash suffix
	h := fnv.New32a()
	h.Write([]byte(nodeName))
	shortHash := fmt.Sprintf("%08x", h.Sum32())

	// extract the identity of the node (e.g. master-0, worker-0)
	match := nodeRolePattern.FindString(nodeName)

	if match != "" {
		// construct the ideal name (e.g. worker-0-a1b2c3d4)
		idealName := fmt.Sprintf("%s-%s", match, shortHash)

		if len(idealName) <= maxLength {
			return idealName
		}

		maxPrefixAllowed := maxLength - len(shortHash) - 1
		if maxPrefixAllowed > 0 && maxPrefixAllowed <= len(match) {
			prefix := sanitizeDNSLabel(match[:maxPrefixAllowed])
			if prefix != "" {
				return fmt.Sprintf("%s-%s", prefix, shortHash)
			}
		}

		if maxLength >= len(shortHash) {
			return shortHash
		}
		return shortHash[:maxLength]
	}

	// fallback for non-standard node name (no worker-X,master-X pattern)
	maxPrefixAllowed := maxLength - len(shortHash) - 1
	if maxPrefixAllowed > 0 && maxPrefixAllowed <= len(nodeName) {
		prefix := sanitizeDNSLabel(nodeName[:maxPrefixAllowed])
		if prefix != "" {
			return fmt.Sprintf("%s-%s", prefix, shortHash)
		}
	} else if maxPrefixAllowed > len(nodeName) && len(nodeName) > 0 {
		prefix := sanitizeDNSLabel(nodeName)
		if prefix != "" {
			return fmt.Sprintf("%s-%s", prefix, shortHash)
		}
	}

	if maxLength >= len(shortHash) {
		return shortHash
	}
	return shortHash[:maxLength]
}

func sanitizeDNSLabel(name string) string {
	// Replace non-DNS characters (dots, underscores, etc.) with dashes
	cleaned := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, name)
	name = cleaned
	name = strings.TrimRight(name, "-")
	name = strings.TrimLeft(name, "-")
	return name
}
