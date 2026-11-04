package pcs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

type alertCommandExecutor func(context.Context, string) (string, string, error)

// ReconcileAlerts repairs missing TNF alerts, script paths, and event filters using a fresh
// CIB observation. Healthy alerts and unrelated alerts are left untouched. Script
// delivery remains MCO's responsibility; repairs wait until the script is executable.
func ReconcileAlerts(ctx context.Context) error {
	return reconcileAlerts(ctx, exec.Execute)
}

func reconcileAlerts(ctx context.Context, execute alertCommandExecutor) error {
	observed, err := collectAlertConfig(ctx, execute)
	if err != nil {
		return err
	}

	var errs []error
	for _, ac := range AlertConfigs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		obs, exists := observed[ac.ID]
		if err := reconcileAlert(ctx, ac, obs, exists, execute); err != nil {
			errs = append(errs, fmt.Errorf("failed to reconcile alert %q: %w", ac.ID, err))
		}
	}
	return errors.Join(errs...)
}

func reconcileAlert(ctx context.Context, ac AlertConfig, observed ObservedAlertConfig, exists bool, execute alertCommandExecutor) error {
	pathMismatch := exists && observed.Path != ac.Path
	// Reapply the filter when needed, including after a previous attempt created
	// the alert but failed before installing its event selection.
	filterMismatch := !exists || observed.SelectFencing != ac.ExpectFencing || observed.SelectNodes != ac.ExpectNodes
	if exists && !pathMismatch && !filterMismatch {
		return nil
	}

	present, err := fileExistsOnHostWithExecutor(ctx, ac.Path, execute)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: %s (MCO rollout pending)", ErrAlertScriptNotPresent, ac.Path)
	}

	// All command arguments come from the operator's fixed AlertConfigs, never
	// from the observed CIB paths or Kubernetes status.
	var cmd string
	if !exists {
		cmd = fmt.Sprintf("/usr/sbin/pcs alert create id=%s path=%s meta timeout=30s", ac.ID, ac.Path)
	} else if pathMismatch {
		cmd = fmt.Sprintf("/usr/sbin/pcs alert update %s path=%s", ac.ID, ac.Path)
	}
	if cmd != "" {
		stdout, stderr, err := execute(ctx, cmd)
		if err != nil {
			return fmt.Errorf("pcs alert repair failed: stdout=%s stderr=%s: %w", stdout, stderr, err)
		}
	}
	if filterMismatch {
		if err := reconcileAlertSelectFilter(ctx, ac, exists, execute); err != nil {
			return err
		}
	}
	klog.Infof("Reconciled pacemaker alert %q", ac.ID)
	return nil
}

func reconcileAlertSelectFilter(ctx context.Context, ac AlertConfig, existed bool, execute alertCommandExecutor) error {
	if !existed {
		return applyAlertSelectFilterWithExecutor(ctx, ac.ID, ac.Path, ac.SelectXML, execute)
	}

	// pcs reports effective event selection, including defaults when <select> is
	// absent. Inspect the XML to distinguish adding a filter from replacing one.
	stdout, stderr, err := execute(ctx, "/usr/sbin/cibadmin --query --scope alerts")
	if err != nil {
		return fmt.Errorf("failed to query alert filters: stderr=%s: %w", stderr, err)
	}
	var alerts struct {
		XMLName xml.Name `xml:"alerts"`
		Alerts  []struct {
			ID     string    `xml:"id,attr"`
			Select *struct{} `xml:"select"`
		} `xml:"alert"`
	}
	if err := xml.Unmarshal([]byte(stdout), &alerts); err != nil {
		return fmt.Errorf("failed to parse alert filters: %w", err)
	}
	for _, alert := range alerts.Alerts {
		if alert.ID != ac.ID {
			continue
		}
		if alert.Select == nil {
			return applyAlertSelectFilterWithExecutor(ctx, ac.ID, ac.Path, ac.SelectXML, execute)
		}
		// --modify merges children and could retain an incorrect event selector.
		// Replace just <select>, preserving the alert's metadata and recipients.
		cmd := fmt.Sprintf(`/usr/sbin/cibadmin --replace --xpath '/cib/configuration/alerts/alert[@id="%s"]/select' --xml-text '<select>%s</select>'`, ac.ID, ac.SelectXML)
		_, stderr, err := execute(ctx, cmd)
		if err != nil {
			return fmt.Errorf("failed to replace alert filter: stderr=%s: %w", stderr, err)
		}
		return nil
	}
	return fmt.Errorf("alert %q disappeared before its filter could be repaired", ac.ID)
}
