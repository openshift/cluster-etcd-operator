# OpenShift etcd load testing

While enabling the higher backend quota sizes, we needed to fill the etcd cluster with several gigabytes worth of data.
This sounds trivial, but has some surprising gotchas when using OpenShift.

This doc gives a viable path to test large quotas, but without crashing your cluster.


## Enabling the quota feature

Currently, the quota is in tech preview mode, so you have to manually enable it via:

```
$ oc apply -f hack/featuregate_quota.yaml
```

The quota can then be set up to 32GiBs using:

```
$ oc patch etcd/cluster --type=merge -p '{"spec": {"backendQuotaGiB": 32}}' 
```


## What doesn't work and why

The main limitation to just create a bunch of objects in etcd is the 2GiB limit on sending and receiving data with
GRPC [1].
Imagine a secret listing across all namespaces call to the kube-apiserver, which is a very common theme many operators
will do in OpenShift (e.g. Prometheus or the Ingress Router to access your metrics and routes with certificates).

etcd can only ever send back 2GiB worth of key/values from such resource key prefix and this limit is reached very
quickly. Assuming we can create a secret with the maximum request size of 1.5MiB, we can put about 1366 secrets before we
hit the GRPC listing limitation.

Another limitation you'll run into when doing this sort of creation/listing is the watch cache memory usage on
apiserver. You can read more about this in [2], but the TL;DR is that the apiserver will allocate about O(watchers*page-size*
object-size*5) bytes of memory. When there are ten watchers listing 2GiB of secrets, it would consume about 100 GiB of memory on the
apiserver. 

You can alleviate some of it by enabling the `ResilientWatchCacheInitialization` feature gate on kube-apiserver, which should become the default starting with OCP 4.21.

The above reasons drive the need to shard across different resources, for maximum flexibility we can use CRDs.
Yet, when creating many thousand CRDs, you may run into another memory limitation: OpenAPISpec conversion.
About ten thousand CRDs will require 32GiB of RAM just to store and convert the OpenAPI definitions. 

Closely related to high watch cache memory consumption is also the kube-controller-manager, which lists and watches all resources in the entire cluster [3].
This is primarily driven by two controllers, the garbage-collector- and the resourcequota-controller. 
Both may need to refresh their caches (e.g. due to restarts on certificate rotation), which causes them to "watch storm" the entire control plane.
If you do not have enough memory to absorb this, you may end up in a cascading thrash fest of the control plane.

### Getting out of the thrash loop

This manifests usually by having two control plane nodes, out of three, having high load, CPU and disk IO due to swapping.
After the first node is down with OOM, it will load over to the second and cause a cascading failure. 
The third will not be impacted anymore, because after the second node the etcd quorum is broken and nothing can be read from the 
last remaining etcd member anymore.

Because these two machines are usually inaccessible at this point, you may need to reset/restart them physically or via KVM.
When you're in a cloud setting, you may also consider to scale-up the machine type to a higher memory SKU at this point to absorb 
the additional memory required by the watch caches.

After a restart, you have a brief period before the kube-apiserver and KCM static pods come up again, so the first step is to move their 
static pod manifest on all control plane nodes:

```
$ sudo mv /etc/kubernetes/manifests/kube-apiserver-pod.yaml .
$ sudo mv /etc/kubernetes/manifests/kube-controller-manager-pod.yaml . 
```

This ensures that etcd can come up on its own, we can verify this by running crictl:

```
$ sudo crictl exec $(sudo crictl ps --name etcdctl -q) etcdctl endpoint status -wtable
+-----------------------+------------------+---------+---------+-----------+------------+-----------+------------+--------------------+--------+
|       ENDPOINT        |        ID        | VERSION | DB SIZE | IS LEADER | IS LEARNER | RAFT TERM | RAFT INDEX | RAFT APPLIED INDEX | ERRORS |
+-----------------------+------------------+---------+---------+-----------+------------+-----------+------------+--------------------+--------+
| https://10.0.0.3:2379 | e881076f3e58fb35 |  3.5.21 |   23 GB |      true |      false |        16 |     422782 |             422782 |        |
| https://10.0.0.5:2379 | 1eb1009e96533fa7 |  3.5.21 |   23 GB |     false |      false |        16 |     422782 |             422782 |        |
| https://10.0.0.6:2379 | 51ff8a825f19c7d2 |  3.5.21 |   23 GB |     false |      false |        16 |     422782 |             422782 |        |
+-----------------------+------------------+---------+---------+-----------+------------+-----------+------------+--------------------+--------+
```

Now, pick a node where you can safely bring the apiserver back again using:

```
$ sudo mv kube-apiserver-pod.yaml /etc/kubernetes/manifests/
```

Wait and observe the memory usage using `free` or `top`. After starting up successfully, the apiserver should respond again from `oc`.
If the memory usage is still not stable for continuous usage, and you're not able to add more RAM, you have to resort to deleting data directly from etcd.

Ensure that the apiserver is shut down again by moving the static pod out as described earlier, then ensure etcd is stable.
In the load testing case, we can very easily delete the CRD prefixes we have created:

```
# contains keys like:
# /kubernetes.io/g3.openshift.com/load-crd-apwzf-8/ceo-load-apwzf-8/ceo-load-inst-apwzf-641
# /kubernetes.io/g3.openshift.com/load-crd-apwzf-8/ceo-load-apwzf-8/ceo-load-inst-apwzf-642

$ DANGER $ sudo crictl exec $(sudo crictl ps --name etcdctl -q) etcdctl del --prefix /kubernetes.io/g3.openshift.com/
```

If you have deleted enough CRDs, you may be able to bring apiserver back up again to start with stable memory usage.

Now, bringing back KCM is the last step. 
You may also try to disable the respective controllers on KCM to avoid memory spikes and spurious watch storms for starters.

This can be done by injecting an `unsupportedConfigOverrides`:
```
$ oc edit kubecontrollermanager.operator.openshift.io/cluster 
spec:
   unsupportedConfigOverrides:
      extendedArguments:
          controllers: ["*","-ttl","-bootstrapsigner","-tokencleaner","selinux-warning-controller","-garbage-collector-controller","-resourcequota-controller"]
```

The minus in front of the controller name will disable them. 

The operator (if still running) will then proceed to roll out a new static pod. In the likely case that nothing will happen, 
because no pods can be scheduled without KCM, you can also resort to editing the latest revision config directly on the node
and then moving the static pod back again:

```
$ sudo vi /etc/kubernetes/static-pod-resources/kube-controller-manager-pod-16/configmaps/config/config.yaml
$ sudo mv kube-controller-manager-pod.yaml /etc/kubernetes/manifests/
```

## KubeBurner

Given we have to be much smarter when it comes to load testing, we can devise a two-step kube burner benchmark.
The first kube-burner run will set up a few hundred CRDs to not overwhelm apiserver, the second will fill each with
objects that contain up to 1.5MiB of string data.

Save the upstream CRD example template below as:

```
cat <<EOF >> example-crd.yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: kubeburners{{.Iteration}}.cloudbulldozer{{.Iteration}}.example.com
spec:
  group: cloudbulldozer{{.Iteration}}.example.com
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                workload:
                  type: string
                iterations:
                  type: integer
  scope: Cluster
  names:
    plural: kubeburners{{.Iteration}}
    singular: kubeburner{{.Iteration}}
    kind: KubeBurner{{.Iteration}}
    shortNames:
    - kb{{.Iteration}}
EOF
```

The following benchmark is our starter:

```
cat <<EOF >> crd-scale.yaml
global:
  gc: false
jobs:
  - name: crd-scale
    jobIterations: 100
    qps: 10
    burst: 10
    namespacedIterations: false
    namespace: crd-scale
    waitWhenFinished: false
    objects:
      - objectTemplate: example-crd.yaml
        replicas: 1
EOF
```

and you can run it with:

```
$ kube-burner init -c crd-scale.yaml 
```

After a quick run, this should give you one hundred CRDs:

```
$ kubectl get crds -oname | grep burner 
customresourcedefinition.apiextensions.k8s.io/kubeburners0.cloudbulldozer0.example.com
...
customresourcedefinition.apiextensions.k8s.io/kubeburners99.cloudbulldozer99.example.com
```

which we need to fill using another two sets of yaml files:

```
cat <<EOF >> example-crd-instance.yaml
apiVersion: cloudbulldozer${i}.example.com/v1
kind: KubeBurner${i}
metadata:
  name: kb-{{.Iteration}}-{{.Replica}}-x
spec:
  workload: "{{ randAlphaNum 10000 }}"
  iterations: {{.Iteration}}
EOF
```

which is our actual resource that's taking the space. Note that we need to template the apiversion and kind with
a shell substitution due to a limitation in kube-burner [4].

Along with a new benchmark:

```
cat <<EOF >> crd-fill.yaml
global:
  gc: false
jobs:
  - name: crd-fill
    jobIterations: 100
    qps: 20
    burst: 20
    namespacedIterations: false
    namespace: crd-fill
    waitWhenFinished: false
    objects:
      {{- range $i , $val := until 100 }}
      - objectTemplate: example-crd-instance-{{ $val }}.yaml
        replicas: 100
      {{- end }}
EOF
```

Then generate all templates and run it with:

```
for i in {0..100}; do
     cat example-crd-instance.yaml | i=$i envsubst > "example-crd-instance-${i}.yaml"
done

$ kube-burner init -c crd-fill.yaml 
```

An important place where etcd / CEO breaks, is the defrag routine. The defrag takes linear time w.r.t. the size of the
database and during the defragmentation the etcd member will not respond to any RPC calls. That may cause liveness
probes to fail and crash loop the etcd instance.

To also test defragmentation, we can run a third step to delete the CRD objects. CEO will then attempt to defrag
automatically:

```
$ kubectl get crd -oname | grep kubeburner | xargs kubectl delete
```

## CEO

The above section on KubeBurner is also built into the CEO commandline utility.
You can run it from the CEO container directly, or alternatively, passing a kubeconfig on your local machine:

```
# uses the in-cluster config
$ cluster-etcd-operator load --g 20
$ cluster-etcd-operator load --g 20 --kubeconfig $KUBECONFIG
```

which will load 20 gigabytes worth of namespaced CRDs and their content into the cluster via the apiserver.
Note, unlike kube-burner, this will not clean up anything, you will have to manually delete the namespaces.

[1] https://github.com/grpc/grpc-go/issues/6623
[2] https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/3157-watch-list
[3] https://github.com/kubernetes/kubernetes/issues/124680
[4] https://github.com/kube-burner/kube-burner/issues/862#issuecomment-2887285321
