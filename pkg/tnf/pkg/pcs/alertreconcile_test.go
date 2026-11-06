package pcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	osexec "os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const alertConfigCommand = "sudo -n /usr/sbin/pcs alert config --output-format json"

type alertCommandResult struct {
	command string
	stdout  string
	stderr  string
	err     error
}

func alertExecutor(t *testing.T, results []alertCommandResult) alertCommandExecutor {
	t.Helper()
	next := 0
	t.Cleanup(func() { require.Equal(t, len(results), next, "not all expected commands were executed") })
	return func(ctx context.Context, command string) (string, string, error) {
		t.Helper()
		require.Less(t, next, len(results), "unexpected command: %s", command)
		result := results[next]
		next++
		require.Equal(t, result.command, command)
		return result.stdout, result.stderr, result.err
	}
}

func alertConfigJSON(t *testing.T, observed map[string]ObservedAlertConfig) string {
	t.Helper()
	alerts := []map[string]interface{}{}
	for id, obs := range observed {
		alerts = append(alerts, map[string]interface{}{
			"id": id, "path": obs.Path,
			"select": map[string]bool{"fencing": obs.SelectFencing, "nodes": obs.SelectNodes},
		})
	}
	data, err := json.Marshal(map[string]interface{}{"alerts": alerts})
	require.NoError(t, err)
	return string(data)
}

func healthyAlertConfig() map[string]ObservedAlertConfig {
	return map[string]ObservedAlertConfig{
		"tnf-taint-alert":   {Path: "/var/lib/pacemaker/alerts/tnf-taint-alert.sh", SelectFencing: true},
		"tnf-untaint-alert": {Path: "/var/lib/pacemaker/alerts/tnf-untaint-alert.sh", SelectNodes: true},
	}
}

func TestReconcileAlertsMissing(t *testing.T) {
	for _, ac := range AlertConfigs {
		t.Run(ac.ID, func(t *testing.T) {
			observed := healthyAlertConfig()
			delete(observed, ac.ID)
			execute := alertExecutor(t, []alertCommandResult{
				{command: alertConfigCommand, stdout: alertConfigJSON(t, observed)},
				{command: "test -x " + ac.Path},
				{command: fmt.Sprintf("sudo -n /usr/sbin/pcs alert create id=%s path=%s meta timeout=30s", ac.ID, ac.Path)},
				{command: fmt.Sprintf(`sudo -n /usr/sbin/cibadmin --modify --xml-text '<alert id="%s" path="%s"><select>%s</select></alert>'`, ac.ID, ac.Path, ac.SelectXML)},
			})
			require.NoError(t, reconcileAlerts(context.Background(), execute))
		})
	}
}

func TestReconcileAlertsPaths(t *testing.T) {
	for _, ac := range AlertConfigs {
		t.Run(ac.ID, func(t *testing.T) {
			observed := healthyAlertConfig()
			obs := observed[ac.ID]
			// Observed paths must never be interpolated into a shell command.
			obs.Path = "/wrong/script.sh; $(touch /tmp/unexpected)"
			observed[ac.ID] = obs
			execute := alertExecutor(t, []alertCommandResult{
				{command: alertConfigCommand, stdout: alertConfigJSON(t, observed)},
				{command: "test -x " + ac.Path},
				{command: fmt.Sprintf("sudo -n /usr/sbin/pcs alert update %s path=%s", ac.ID, ac.Path)},
			})
			require.NoError(t, reconcileAlerts(context.Background(), execute))
		})
	}
}

func TestReconcileAlertsHealthy(t *testing.T) {
	observed := healthyAlertConfig()
	observed["custom-alert"] = ObservedAlertConfig{Path: "/custom.sh"}
	execute := alertExecutor(t, []alertCommandResult{{command: alertConfigCommand, stdout: alertConfigJSON(t, observed)}})
	require.NoError(t, reconcileAlerts(context.Background(), execute))
}

func TestReconcileAlertsSwappedPaths(t *testing.T) {
	observed := healthyAlertConfig()
	taint, untaint := observed["tnf-taint-alert"], observed["tnf-untaint-alert"]
	taint.Path, untaint.Path = untaint.Path, taint.Path
	observed["tnf-taint-alert"], observed["tnf-untaint-alert"] = taint, untaint
	commands := []alertCommandResult{{command: alertConfigCommand, stdout: alertConfigJSON(t, observed)}}
	for _, ac := range AlertConfigs {
		commands = append(commands,
			alertCommandResult{command: "test -x " + ac.Path},
			alertCommandResult{command: fmt.Sprintf("sudo -n /usr/sbin/pcs alert update %s path=%s", ac.ID, ac.Path)},
		)
	}
	require.NoError(t, reconcileAlerts(context.Background(), alertExecutor(t, commands)))
}

func TestReconcileAlertsFilters(t *testing.T) {
	for _, ac := range AlertConfigs {
		t.Run(ac.ID, func(t *testing.T) {
			observed := healthyAlertConfig()
			observed[ac.ID] = ObservedAlertConfig{Path: ac.Path, SelectFencing: !ac.ExpectFencing, SelectNodes: !ac.ExpectNodes}
			for _, hasSelect := range []bool{false, true} {
				t.Run(fmt.Sprintf("hasSelect=%t", hasSelect), func(t *testing.T) {
					filter := ""
					if hasSelect {
						filter = "<select><select_nodes/><select_fencing/></select>"
					}
					alertXML := fmt.Sprintf(`<alerts><alert id="%s" path="%s">%s<recipient id="keep-me" value="keep-me"/></alert></alerts>`, ac.ID, ac.Path, filter)
					repairCommand := fmt.Sprintf(`sudo -n /usr/sbin/cibadmin --modify --xml-text '<alert id="%s" path="%s"><select>%s</select></alert>'`, ac.ID, ac.Path, ac.SelectXML)
					if hasSelect {
						repairCommand = fmt.Sprintf(`sudo -n /usr/sbin/cibadmin --replace --xpath '/cib/configuration/alerts/alert[@id="%s"]/select' --xml-text '<select>%s</select>'`, ac.ID, ac.SelectXML)
					}
					execute := alertExecutor(t, []alertCommandResult{
						{command: alertConfigCommand, stdout: alertConfigJSON(t, observed)},
						{command: "test -x " + ac.Path},
						{command: "sudo -n /usr/sbin/cibadmin --query --scope alerts", stdout: alertXML},
						{command: repairCommand},
					})
					require.NoError(t, reconcileAlerts(context.Background(), execute))
				})
			}
		})
	}
}

func TestReconcileAlertFilterFailures(t *testing.T) {
	ac := AlertConfigs[0]
	failure := errors.New("CIB failure")
	for _, tc := range []struct {
		name       string
		queryXML   string
		queryError error
		replace    bool
	}{
		{name: "query failed", queryError: failure},
		{name: "invalid XML", queryXML: "not XML"},
		{name: "unexpected root", queryXML: "<cib/>"},
		{name: "alert disappeared", queryXML: "<alerts/>"},
		{name: "replace failed", queryXML: `<alerts><alert id="tnf-taint-alert"><select/></alert></alerts>`, replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commands := []alertCommandResult{{command: "sudo -n /usr/sbin/cibadmin --query --scope alerts", stdout: tc.queryXML, err: tc.queryError}}
			if tc.replace {
				commands = append(commands, alertCommandResult{
					command: fmt.Sprintf(`sudo -n /usr/sbin/cibadmin --replace --xpath '/cib/configuration/alerts/alert[@id="%s"]/select' --xml-text '<select>%s</select>'`, ac.ID, ac.SelectXML),
					err:     failure,
				})
			}
			err := reconcileAlertSelectFilter(context.Background(), ac, true, alertExecutor(t, commands))
			require.Error(t, err)
			if tc.queryError != nil || tc.replace {
				require.ErrorIs(t, err, failure)
			}
		})
	}
}

func TestReconcileAlertsObservationFailure(t *testing.T) {
	for _, output := range []string{"not JSON", "{}", "null", `{"alerts":null}`} {
		t.Run(output, func(t *testing.T) {
			execute := alertExecutor(t, []alertCommandResult{{command: alertConfigCommand, stdout: output}})
			require.Error(t, reconcileAlerts(context.Background(), execute))
		})
	}
	failure := errors.New("CIB unavailable")
	execute := alertExecutor(t, []alertCommandResult{{command: alertConfigCommand, stderr: "unavailable", err: failure}})
	require.ErrorIs(t, reconcileAlerts(context.Background(), execute), failure)
}

func TestReconcileAlertsRepairFailures(t *testing.T) {
	ac := AlertConfigs[0]
	failure := errors.New("repair failed")
	for _, stage := range []string{"script check", "script missing", "create", "path update", "filter"} {
		t.Run(stage, func(t *testing.T) {
			observed := healthyAlertConfig()
			delete(observed, ac.ID)
			if stage == "path update" {
				observed[ac.ID] = ObservedAlertConfig{Path: "/wrong.sh", SelectFencing: true}
			}
			commands := []alertCommandResult{
				{command: alertConfigCommand, stdout: alertConfigJSON(t, observed)},
				{command: "test -x " + ac.Path},
			}
			switch stage {
			case "script check":
				commands[1].err = failure
			case "script missing":
				commands[1].err = &osexec.ExitError{}
			case "create", "path update", "filter":
				cmd := fmt.Sprintf("sudo -n /usr/sbin/pcs alert create id=%s path=%s meta timeout=30s", ac.ID, ac.Path)
				if stage == "path update" {
					cmd = fmt.Sprintf("sudo -n /usr/sbin/pcs alert update %s path=%s", ac.ID, ac.Path)
				}
				commands = append(commands, alertCommandResult{command: cmd, stderr: "failed", err: failure})
				if stage == "filter" {
					commands[2].err = nil
					commands = append(commands, alertCommandResult{
						command: fmt.Sprintf(`sudo -n /usr/sbin/cibadmin --modify --xml-text '<alert id="%s" path="%s"><select>%s</select></alert>'`, ac.ID, ac.Path, ac.SelectXML),
						stderr:  "failed", err: failure,
					})
				}
			}
			err := reconcileAlerts(context.Background(), alertExecutor(t, commands))
			if stage == "script missing" {
				require.ErrorIs(t, err, ErrAlertScriptNotPresent)
			} else {
				require.ErrorIs(t, err, failure)
			}
			require.ErrorContains(t, err, ac.ID)
		})
	}
}

func TestReconcileAlertsRetriesPartialCreation(t *testing.T) {
	ac := AlertConfigs[0]
	missing := healthyAlertConfig()
	delete(missing, ac.ID)
	partial := healthyAlertConfig()
	partial[ac.ID] = ObservedAlertConfig{Path: ac.Path, SelectNodes: true}
	filterCommand := fmt.Sprintf(`sudo -n /usr/sbin/cibadmin --modify --xml-text '<alert id="%s" path="%s"><select>%s</select></alert>'`, ac.ID, ac.Path, ac.SelectXML)
	failure := errors.New("temporary filter failure")
	execute := alertExecutor(t, []alertCommandResult{
		{command: alertConfigCommand, stdout: alertConfigJSON(t, missing)},
		{command: "test -x " + ac.Path},
		{command: fmt.Sprintf("sudo -n /usr/sbin/pcs alert create id=%s path=%s meta timeout=30s", ac.ID, ac.Path)},
		{command: filterCommand, err: failure},
		// On the next run, creation has persisted but the filter is still absent.
		{command: alertConfigCommand, stdout: alertConfigJSON(t, partial)},
		{command: "test -x " + ac.Path},
		{command: "sudo -n /usr/sbin/cibadmin --query --scope alerts", stdout: fmt.Sprintf(`<alerts><alert id="%s" path="%s"/></alerts>`, ac.ID, ac.Path)},
		{command: filterCommand},
		// Once the CIB confirms recovery, reconciliation becomes read-only.
		{command: alertConfigCommand, stdout: alertConfigJSON(t, healthyAlertConfig())},
	})
	require.ErrorIs(t, reconcileAlerts(context.Background(), execute), failure)
	require.NoError(t, reconcileAlerts(context.Background(), execute))
	require.NoError(t, reconcileAlerts(context.Background(), execute))
}

func TestReconcileAlertsContinuesAfterFailure(t *testing.T) {
	failure := errors.New("script check failed")
	var repaired []string
	execute := func(ctx context.Context, command string) (string, string, error) {
		switch command {
		case alertConfigCommand:
			return `{"alerts":[]}`, "", nil
		case "test -x " + AlertConfigs[0].Path:
			return "", "", failure
		default:
			if strings.Contains(command, "alert create") {
				repaired = append(repaired, command)
			}
			return "", "", nil
		}
	}
	require.ErrorIs(t, reconcileAlerts(context.Background(), execute), failure)
	require.Len(t, repaired, 1)
	require.Contains(t, repaired[0], "tnf-untaint-alert")
}

func TestReconcileAlertsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	execute := func(ctx context.Context, command string) (string, string, error) {
		require.Equal(t, alertConfigCommand, command)
		cancel()
		return `{"alerts":[]}`, "", nil
	}
	require.ErrorIs(t, reconcileAlerts(ctx, execute), context.Canceled)
}
