# OpenShift etcd load testing

While enabling the higher backend quota sizes, we needed to fill the etcd cluster with several gigabytes worth of data.
This sounds trivial, but has some surprising gotchas when using OpenShift.

This doc gives a viable path to test large quotas, but without crashing your cluster.

## What doesn't work and why

The main limitation to just create a bunch of objects in etcd is the 2GiB limit on sending and receiving data with
GRPC [1].
Imagine a secret listing across all namespaces call to the kube-apiserver, which is a very common theme many operators
will do in OpenShift (e.g. Prometheus or the Ingress Router to access your metrics and routes with certificates).

etcd can only ever send back 2GiB worth of key/values from such resource key prefix and this limit is reached very
quickly.
Assuming we can create a secret with the maximum request size of 1.5MiB, we can put about 1366 secrets before we
hit the GRPC listing limitation.

Another limitation you'll run into when doing this sort of creation/listing is the watch cache memory usage on
apiserver.
You can read more about this in [2], but the TL;DR is that the apiserver will allocate about O(watchers*page-size*
object-size*5)
bytes of memory. When there are ten watchers listing 2GiB of secrets, it would consume about 100 GiB of memory on the
apiserver.

The above reasons drive the need to shard across different resources, and we're glad to have CRDs (custom resource
definitions).
Yet, when creating many thousand CRDs, you may run into another memory limitation: OpenAPISpec conversion.
About ten thousand CRDs will require 32GiB of RAM just to store and convert the OpenAPI definitions. Lack of RAM means
your control plane will thrash into a long winding and cascading death loop trying to swap in and out enough memory.

## Enabling the quota feature

Currently, the quota is in tech preview mode, so you have to manually enable it via:

```
$ oc apply -f hack/featuregate_quota.yaml
```

The quota can then be set up to 32GiBs using:

```
$ oc patch etcd/cluster --type=merge -p '{"spec": {"backendQuotaGiB": 32}}' 
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
a shell substitution due to a limitation in kube-burner [3].

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
probes
to fail and crash loop the etcd instance.

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
[3] https://github.com/kube-burner/kube-burner/issues/862#issuecomment-2887285321