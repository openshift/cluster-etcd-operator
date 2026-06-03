package pcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	osexec "os/exec"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

type alertConfig struct {
	id        string
	path      string
	selectXML string // CIB <select> element content to filter event types
}

var alertConfigs = []alertConfig{
	{
		id:        "tnf-taint-alert",
		path:      "/var/lib/pacemaker/alerts/tnf-taint-alert.sh",
		selectXML: "<select_fencing/>",
	},
	{
		id:        "tnf-untaint-alert",
		path:      "/var/lib/pacemaker/alerts/tnf-untaint-alert.sh",
		selectXML: "<select_nodes/><select_attributes/>",
	},
}

// ConfigureAlerts registers pacemaker alert agents for fencing taint/untaint.
// It is idempotent: existing alerts are deleted and re-created to handle upgrades.
// Returns an error if alert scripts are not yet on disk (MCO rollout pending),
// which causes the calling job to retry.
func ConfigureAlerts(ctx context.Context) error {
	klog.Info("Configuring pacemaker alert agents")

	for _, ac := range alertConfigs {
		if err := configureAlert(ctx, ac); err != nil {
			return err
		}
	}

	klog.Info("Pacemaker alert agent configuration succeeded")
	return nil
}

func configureAlert(ctx context.Context, ac alertConfig) error {
	scriptPresent, err := fileExistsOnHost(ctx, ac.path)
	if err != nil {
		return fmt.Errorf("failed to check for alert script %s: %w", ac.path, err)
	}
	if !scriptPresent {
		return fmt.Errorf("alert script %s not yet present (MCO rollout pending), will retry", ac.path)
	}

	exists, err := alertExists(ctx, ac.id)
	if err != nil {
		return fmt.Errorf("failed to check existing alert %q: %w", ac.id, err)
	}

	if exists {
		klog.Infof("Alert %q already exists, deleting for re-creation", ac.id)
		deleteCmd := fmt.Sprintf("/usr/sbin/pcs alert delete %s", ac.id)
		if _, stdErr, err := exec.Execute(ctx, deleteCmd); err != nil {
			return fmt.Errorf("failed to delete existing alert %q: %s: %w", ac.id, stdErr, err)
		}
	}

	createCmd := fmt.Sprintf("/usr/sbin/pcs alert create id=%s path=%s meta timeout=30s", ac.id, ac.path)
	stdOut, stdErr, err := exec.Execute(ctx, createCmd)
	if err != nil {
		return fmt.Errorf("failed to create alert %q: stdout=%s stderr=%s: %w", ac.id, stdOut, stdErr, err)
	}

	if ac.selectXML != "" {
		if err := applyAlertSelectFilter(ctx, ac.id, ac.selectXML); err != nil {
			return fmt.Errorf("failed to apply select filter for alert %q: %w", ac.id, err)
		}
	}

	klog.Infof("Successfully configured pacemaker alert %q", ac.id)
	return nil
}

// applyAlertSelectFilter adds a <select> element to an alert via cibadmin.
// pcs does not expose select filters, so we modify the CIB directly.
func applyAlertSelectFilter(ctx context.Context, alertID, selectContent string) error {
	selectXML := fmt.Sprintf(`<select>%s</select>`, selectContent)
	cmd := fmt.Sprintf("/usr/sbin/cibadmin --modify --xpath //alerts/alert[@id='%s'] --xml-text '%s'", alertID, selectXML)
	_, stdErr, err := exec.Execute(ctx, cmd)
	if err != nil {
		return fmt.Errorf("cibadmin modify for alert %q failed: %s: %w", alertID, stdErr, err)
	}
	klog.Infof("Applied select filter to alert %q: %s", alertID, selectContent)
	return nil
}

// pcsAlertConfigOutput represents the JSON output of `pcs alert config --output-format json`.
type pcsAlertConfigOutput struct {
	Alerts []struct {
		ID string `json:"id"`
	} `json:"alerts"`
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
	cmd := fmt.Sprintf("test -x %s", path)
	_, _, err := exec.Execute(ctx, cmd)
	if err != nil {
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check file %s on host: %w", path, err)
	}
	return true, nil
}
