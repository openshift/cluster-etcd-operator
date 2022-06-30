⚠⚠⚠ THIS IS A LIVING DOCUMENT AND LIKELY TO CHANGE QUICKLY ⚠⚠⚠

# Hacking on the CEO

### Maintenance

#### Update Alerts

Alerts are stored in `jsonnet/custom.libsonnet`, you need to update the respective `prometheusRules` in there and then re-generate the manifest yamls via:

> hack/generate.sh

#### CVO resource removal
Any resource in the manifests/ directory is managed by CVO. Removal of these resources requires code to be added 
that removes these resources frequently[1]. This must be maintained for 1 release then the code can be removed.

[1] https://github.com/openshift/cluster-etcd-operator/pull/429

#### Controller removal
Anytime we remove a controller we should also ensure that we cleanup any stale status conditions. Leaving these
statuses to linger can result in degraded controllers that could block upgrade[1],[2].

example:

```go
	staleConditions := staleconditions.NewRemoveStaleConditionsController(
		[]string{
			// IP migrator was removed in 4.5, however older clusters can still have this condition set and it might prevent upgrades
			"EtcdMemberIPMigratorDegraded",
		},
		operatorClient,
		controllerContext.EventRecorder,
	)
```
[1] https://bugzilla.redhat.com/show_bug.cgi?id=1851351

[2] https://github.com/openshift/cluster-etcd-operator/pull/517
