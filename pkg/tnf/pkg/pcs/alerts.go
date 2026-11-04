package pcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	osexec "os/exec"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// ErrAlertScriptNotPresent indicates the alert script has not yet been delivered
// to the host by MCO. Callers can check for this with errors.Is to distinguish
// it from other, unexpected failures.
var ErrAlertScriptNotPresent = errors.New("alert script not yet present")

// AlertConfig describes the expected CIB registration of a single pacemaker alert agent.
// It is exported so the status collector (pacemaker package) can compare observed CIB state
// against the expected path and event filter for each known alert agent.
type AlertConfig struct {
	ID        string
	Path      string
	SelectXML string // CIB <select> element content to filter event types
	// ExpectFencing is true when the alert is expected to subscribe to fencing events
	// (i.e. its <select> contains <select_fencing/>).
	ExpectFencing bool
	// ExpectNodes is true when the alert is expected to subscribe to node events
	// (i.e. its <select> contains <select_nodes/>).
	ExpectNodes bool
}

// AlertConfigs is the set of pacemaker alert agents TNF manages.
var AlertConfigs = []AlertConfig{
	{
		ID:            "tnf-taint-alert",
		Path:          "/var/lib/pacemaker/alerts/tnf-taint-alert.sh",
		SelectXML:     "<select_fencing/>",
		ExpectFencing: true,
	},
	{
		ID:          "tnf-untaint-alert",
		Path:        "/var/lib/pacemaker/alerts/tnf-untaint-alert.sh",
		SelectXML:   "<select_nodes/>",
		ExpectNodes: true,
	},
}

// ObservedAlertConfig is the subset of an alert's CIB registration that the status collector
// inspects to decide Configured vs Misconfigured: the script path and the event-filter booleans
// that `pcs alert config --output-format json` reports under "select".
type ObservedAlertConfig struct {
	Path          string
	SelectFencing bool
	SelectNodes   bool
}

// ConfigureAlerts registers pacemaker alert agents for fencing taint/untaint.
// It is idempotent: existing alerts are updated in-place via cibadmin --modify
// to avoid a race window where the alert is temporarily absent.
// Returns an error if alert scripts are not yet on disk (MCO rollout pending),
// which causes the calling job to retry.
func ConfigureAlerts(ctx context.Context) error {
	klog.Info("Configuring pacemaker alert agents")

	for _, ac := range AlertConfigs {
		if err := configureAlert(ctx, ac); err != nil {
			return err
		}
	}

	klog.Info("Pacemaker alert agent configuration succeeded")
	return nil
}

// AllAlertsConfigured returns true if every known pacemaker alert agent is
// already registered in the CIB.
func AllAlertsConfigured(ctx context.Context) (bool, error) {
	for _, ac := range AlertConfigs {
		exists, err := alertExists(ctx, ac.ID)
		if err != nil {
			return false, fmt.Errorf("failed to check existing alert %q: %w", ac.ID, err)
		}
		if !exists {
			return false, nil
		}
	}
	return true, nil
}

// ConfigureAlertsWithRetry ensures the pacemaker alert agents are registered,
// tolerating the alert scripts not yet being present on-node (they are delivered
// asynchronously by MCO, which can lag behind an
// etcd-operator upgrade). It skips entirely if the alerts are already configured,
// and otherwise polls until they can be configured or the timeout is reached.
func ConfigureAlertsWithRetry(ctx context.Context) error {
	alreadyConfigured, err := AllAlertsConfigured(ctx)
	if err != nil {
		return fmt.Errorf("failed to check existing alert configuration: %w", err)
	}
	if alreadyConfigured {
		klog.Info("Pacemaker alert agents are already configured, skipping registration")
		return nil
	}

	return wait.PollUntilContextTimeout(ctx, tools.JobPollInterval, tools.AlertConfigurationTimeout, true, func(ctx context.Context) (bool, error) {
		if err := ConfigureAlerts(ctx); err != nil {
			if errors.Is(err, ErrAlertScriptNotPresent) {
				klog.Warningf("Pacemaker alert agent configuration not yet possible, will retry: %v", err)
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

func configureAlert(ctx context.Context, ac AlertConfig) error {
	scriptPresent, err := fileExistsOnHost(ctx, ac.Path)
	if err != nil {
		return fmt.Errorf("failed to check for alert script %s: %w", ac.Path, err)
	}
	if !scriptPresent {
		return fmt.Errorf("%w: %s (MCO rollout pending)", ErrAlertScriptNotPresent, ac.Path)
	}

	exists, err := alertExists(ctx, ac.ID)
	if err != nil {
		return fmt.Errorf("failed to check existing alert %q: %w", ac.ID, err)
	}

	if exists {
		klog.Infof("Alert %q already exists, updating in-place", ac.ID)
		if err := modifyAlert(ctx, ac); err != nil {
			return fmt.Errorf("failed to update alert %q in-place: %w", ac.ID, err)
		}
	} else {
		createCmd := fmt.Sprintf("/usr/sbin/pcs alert create id=%s path=%s meta timeout=30s", ac.ID, ac.Path)
		stdOut, stdErr, err := exec.Execute(ctx, createCmd)
		if err != nil {
			return fmt.Errorf("failed to create alert %q: stdout=%s stderr=%s: %w", ac.ID, stdOut, stdErr, err)
		}
		if ac.SelectXML != "" {
			if err := applyAlertSelectFilter(ctx, ac.ID, ac.Path, ac.SelectXML); err != nil {
				return fmt.Errorf("failed to apply select filter for alert %q: %w", ac.ID, err)
			}
		}
	}

	klog.Infof("Successfully configured pacemaker alert %q", ac.ID)
	return nil
}

// modifyAlert atomically updates an existing alert via cibadmin --modify,
// setting path, meta timeout, and select filter in a single CIB write.
func modifyAlert(ctx context.Context, ac AlertConfig) error {
	metaXML := fmt.Sprintf(
		`<meta_attributes id="%s-meta_attributes">`+
			`<nvpair id="%s-meta_attributes-timeout" name="timeout" value="30s"/>`+
			`</meta_attributes>`,
		ac.ID, ac.ID,
	)
	selectXML := ""
	if ac.SelectXML != "" {
		selectXML = fmt.Sprintf("<select>%s</select>", ac.SelectXML)
	}
	alertXML := fmt.Sprintf(`<alert id="%s" path="%s">%s%s</alert>`, ac.ID, ac.Path, metaXML, selectXML)
	cmd := fmt.Sprintf("/usr/sbin/cibadmin --modify --xml-text '%s'", alertXML)
	_, stdErr, err := exec.Execute(ctx, cmd)
	if err != nil {
		return fmt.Errorf("cibadmin modify for alert %q failed: %s: %w", ac.ID, stdErr, err)
	}
	return nil
}

// applyAlertSelectFilter adds a <select> element to an alert via cibadmin.
// pcs does not expose select filters, so we modify the CIB directly.
// We use cibadmin --modify with the full <alert> element (matched by id attribute)
// rather than --xpath, because --xpath with a child xml-text that doesn't match
// the target element type fails with "No such device or address" on Pacemaker 2.1.x.
func applyAlertSelectFilter(ctx context.Context, alertID, alertPath, selectContent string) error {
	return applyAlertSelectFilterWithExecutor(ctx, alertID, alertPath, selectContent, exec.Execute)
}

func applyAlertSelectFilterWithExecutor(ctx context.Context, alertID, alertPath, selectContent string, execute alertCommandExecutor) error {
	alertXML := fmt.Sprintf(`<alert id="%s" path="%s"><select>%s</select></alert>`, alertID, alertPath, selectContent)
	cmd := fmt.Sprintf("/usr/sbin/cibadmin --modify --xml-text '%s'", alertXML)
	_, stdErr, err := execute(ctx, cmd)
	if err != nil {
		return fmt.Errorf("cibadmin modify for alert %q failed: %s: %w", alertID, stdErr, err)
	}
	klog.Infof("Applied select filter to alert %q: %s", alertID, selectContent)
	return nil
}

// pcsAlertConfigOutput represents the JSON output of `pcs alert config --output-format json`.
// The path and select fields are already emitted by pcs; the collector unmarshals them to
// distinguish a correctly-registered alert from a Misconfigured one (wrong path/event filter).
type pcsAlertConfigOutput struct {
	Alerts []struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		Select struct {
			Nodes   bool `json:"nodes"`
			Fencing bool `json:"fencing"`
		} `json:"select"`
	} `json:"alerts"`
}

// CollectAlertConfig runs `pcs alert config --output-format json` once and returns the observed
// configuration for every currently-registered alert, keyed by alert id. It returns a nil map and
// a non-nil error if the command itself fails or its output cannot be parsed - "not observed this
// run", which the status collector treats as carry-forward/Pending rather than "confirmed missing".
func CollectAlertConfig(ctx context.Context) (map[string]ObservedAlertConfig, error) {
	return collectAlertConfig(ctx, exec.Execute)
}

func collectAlertConfig(ctx context.Context, execute alertCommandExecutor) (map[string]ObservedAlertConfig, error) {
	cmd := "/usr/sbin/pcs alert config --output-format json"
	stdOut, stdErr, err := execute(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("pcs alert config failed: stderr=%s: %w", stdErr, err)
	}

	var config pcsAlertConfigOutput
	if err := json.Unmarshal([]byte(stdOut), &config); err != nil {
		return nil, fmt.Errorf("failed to parse pcs alert config JSON: %w", err)
	}
	if config.Alerts == nil {
		return nil, fmt.Errorf("pcs alert config JSON does not contain an alerts array")
	}

	observed := make(map[string]ObservedAlertConfig, len(config.Alerts))
	for _, alert := range config.Alerts {
		observed[alert.ID] = ObservedAlertConfig{
			Path:          alert.Path,
			SelectFencing: alert.Select.Fencing,
			SelectNodes:   alert.Select.Nodes,
		}
	}
	return observed, nil
}

func alertExists(ctx context.Context, alertID string) (bool, error) {
	cmd := "/usr/sbin/pcs alert config --output-format json"
	stdOut, stdErr, err := exec.Execute(ctx, cmd)
	if err != nil {
		return false, fmt.Errorf("pcs alert config failed: stderr=%s: %w", stdErr, err)
	}

	var config pcsAlertConfigOutput
	if err := json.Unmarshal([]byte(stdOut), &config); err != nil {
		return false, fmt.Errorf("failed to parse pcs alert config JSON: %w", err)
	}

	for _, alert := range config.Alerts {
		if alert.ID == alertID {
			return true, nil
		}
	}
	return false, nil
}

func fileExistsOnHost(ctx context.Context, path string) (bool, error) {
	return fileExistsOnHostWithExecutor(ctx, path, exec.Execute)
}

func fileExistsOnHostWithExecutor(ctx context.Context, path string, execute alertCommandExecutor) (bool, error) {
	cmd := fmt.Sprintf("test -x %s", path)
	_, _, err := execute(ctx, cmd)
	if err != nil {
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check file %s on host: %w", path, err)
	}
	return true, nil
}
