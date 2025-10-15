// Package pacemaker provides health monitoring and status collection for Pacemaker-managed
// clusters in OpenShift's ExternalEtcd deployment topology.
//
// # Architecture
//
// The package consists of two main components:
//
//  1. Status Collector (statuscollector.go): Collects raw Pacemaker cluster status by executing
//     "pcs status xml" and storing the results in a Pacemaker custom resource. The collector
//     operates on a "collect everything" principle - it writes all data as-is without validation,
//     allowing the health check to make informed decisions based on available data.
//
//  2. Health Check Controller (healthcheck.go): Monitors the Pacemaker CR and evaluates
//     cluster health. It is hardened to handle missing or incomplete data gracefully, treating
//     absent information as "unknown" rather than "error". The health check updates operator
//     conditions and records events based on cluster state.
//
// # Security Considerations
//
//   - Command execution is hardcoded (no user input) and uses sudo -n (non-interactive)
//   - XML parsing includes size limits (10MB) to prevent memory exhaustion
//   - Command execution has timeouts to prevent hanging
//   - Raw XML is stored in CRs but never logged to prevent information disclosure
//
// # Design Principles
//
//   - Separation of concerns: Collection is separate from health evaluation
//   - Missing data = unknown status, not error: Absence of information doesn't indicate failure
//   - Type-safe enums: Use typed constants for all enumerated values
//   - Defensive programming: All pointer dereferences are guarded with nil checks
//   - Fail-safe: Transient collection failures don't immediately mark the cluster as degraded
package pacemaker
