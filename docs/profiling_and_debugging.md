# Profiling and debugging CEO and etcd

This doc mentions details of the official [Go diagnostics documentation](https://go.dev/doc/diagnostics) and the [pprof blog post](https://go.dev/blog/pprof) and adds some OpenShift and etcd specific knowledge to it. 
We're making primarily use of the [pprof http handlers](https://pkg.go.dev/net/http/pprof) that are available through library-go in the operator and like-wise in etcd itself. 

Then, once we got all data from the remote location we are able to analyse with the standard go toolkits on the local machine.

This has been tested with a nightly 4.11 build of OpenShift on AWS, the tools used should be available on any other recent OpenShift version.

## Connecting to CEO

There are two paths available:
* port-forwarding the remote pod debug ports to local 
* running all commands remote and transferring the data to local

For CEO the port-forwarding is the most convenient, for etcd it's easier to run the commands directly on the shell and copy the results back due to the certificate handling.

For everything we need a pod, so let's establish an env variable:

> POD_NAME=$(kubectl get pods -n openshift-etcd-operator -oname)
>
> pod/etcd-operator-5d7685495f-6f86t

It's also worth looking into the beginning of the pod log, you will find the following log entry that reveals the port and endpoints:

> I0629 08:53:50.032127 1 profiler.go:21] Starting profiling endpoint at http://127.0.0.1:6060/debug/pprof/

### Port Forwarding

Using the pod name and the port, we can easily proxy this to our local machine using:

> kubectl port-forward $POD_NAME -n openshift-etcd-operator 6060:6060

and subsequently on your local machine:

> curl http://127.0.0.1:6060/debug/pprof/

should show you a very basic html file that gives you an overview of all available endpoints to tap into. Alternatively open the page in your browser to see it in your full glory.

### Terminal with exec

Instead of port-forwarding, you can either use the OpenShift console to open a terminal window in the browser, or you can exec into the pod directly using:

> kubectl exec -it $POD_NAME -n openshift-etcd-operator -- /bin/sh

On either console, you should be able to run curl:

> curl http://127.0.0.1:6060/debug/pprof/

which should show you a very basic html file that gives you an overview of all available endpoints to tap into. Go look at those that are available, there are plenty more useful tools than we are going to cover in this document.

#### Getting data out of the terminal

Since RHCOS is locked down heavily there are basically no other tools available than curl. In order to get files out of the pod, you can use `oc rsync` like that:

> oc rsync -n openshift-etcd-operator $POD_NAME:/var/run/secrets/serving-cert .

which for example downloads the serving certificates to your current local folder.

Alternatively you can also try [`transfer.sh`](https://transfer.sh/) which allows you to upload a file with curl and download it with a generated link afterwards. Be careful what you share through this service and **definitely do not upload memory dumps or any other information that could leak credentials**.

## Dumping Goroutines

To debug deadlocks or stuck goroutines, we can grab a goroutine dump (similar to a threaddump in Java):

> curl http://127.0.0.1:6060/debug/pprof/goroutine?debug=1

`debug=1` here generates a stack trace through profiling for a short period, using `debug=2` will give you additionally a full goroutine stack dump from the runtime.

You can look at the resulting text output and see what you're goroutines are doing.


## CPU profile

To understand where our precious CPU time goes (on etcd it's always TLS handshakes :wink:), we can run a sample-based profiler. For that you need a normal golang installation:

> go tool pprof localhost:6060/debug/pprof/profile?seconds=30

alternatively you can run the same command using curl and a file output:

> curl http://localhost:6060/debug/pprof/profile?seconds=30 -o cpu.profile

and then copy the resulting `cpu.profile` and analyse on your local machine using `go tool pprof`. 

`go tool pprof` is normally a console tool with a prompt, so you can run commands like `top5` to get the top 5 calls that take the most time. What's more convenient however is the web interface which you can enable with:

> go tool pprof -http localhost:8080 localhost:6060/debug/pprof/profile?seconds=30

which opens a browser window to `localhost:8080` (ensure the port is available) and interactively lets you browse the performance profile using flame graphs and other fancy visualizations.

## Memory profile

Similar to CPU profiling, you can also gather information about the heap and its allocations. This is useful to track down memory leaks or to understand why memory increased in a certain situation.

The tooling works exactly the same as in the CPU profiling, the endpoint is the only difference:

> go tool pprof -http localhost:8080 localhost:6060/debug/pprof/heap?seconds=30

will open a web interface with a 30s heap profile. 


## Profiling and debugging etcd

You can debug and profile a live running etcd the exact same way as the CEO. 
The only difference is the usage of mTLS on the 2379 port, which makes using curl and `go tool pprof` a little more cumbersome.


Here we're grabbing the first etcd pod returned by kubectl as our pod:

> POD_NAME=$(kubectl get pods -n openshift-etcd -oname | grep etcd-ip | head -n1)
>
> pod/etcd-ip-10-0-128-94.us-west-1.compute.internal

Subsequently the commands that were run above are using a different protocol (https, port 2379), namespace, a container filter for etcd maincar and paramaters for the certificates.

### Copying the certificates

etcd starts with certificates from the etcd-all-certs folder, usually there is a key and crt file for each of the control plane nodes. Let's copy all of them to the local machine:

> oc rsync -n openshift-etcd -c etcd $POD_NAME:/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/ .

You should see files like this:

```
etcd-peer-ip-10-0-128-94.us-west-1.compute.internal.crt
etcd-peer-ip-10-0-128-94.us-west-1.compute.internal.key
etcd-peer-ip-10-0-131-239.us-west-1.compute.internal.crt
etcd-peer-ip-10-0-131-239.us-west-1.compute.internal.key
etcd-peer-ip-10-0-220-159.us-west-1.compute.internal.crt
etcd-peer-ip-10-0-220-159.us-west-1.compute.internal.key
etcd-serving-ip-10-0-128-94.us-west-1.compute.internal.crt
etcd-serving-ip-10-0-128-94.us-west-1.compute.internal.key
etcd-serving-ip-10-0-131-239.us-west-1.compute.internal.crt
etcd-serving-ip-10-0-131-239.us-west-1.compute.internal.key
etcd-serving-ip-10-0-220-159.us-west-1.compute.internal.crt
etcd-serving-ip-10-0-220-159.us-west-1.compute.internal.key
```

We're only interested in the serving certificates for the respective node, as those are used for running the server on port 2379.

### Port forwarding

The port forwarding works similar, you only have to target a different namespace and the well known etcd port:

> kubectl port-forward $POD_NAME -n openshift-etcd 2379:2379

You should then be able to curl the pprof endpoint using the certificates like this:

> curl -k --key etcd-serving-ip-10-0-128-94.us-west-1.compute.internal.key --cert etcd-serving-ip-10-0-128-94.us-west-1.compute.internal.crt https://127.0.0.1:2379/debug/pprof/

### Go Tool and Certificates

`go tool` has quite a bit of [history](https://github.com/golang/go/issues/20939) [with self-signed certs](https://github.com/golang/go/issues/11468).

In an ideal world, you should be able to run:

> go tool pprof -tls_key etcd-serving-ip-10-0-128-94.us-west-1.compute.internal.key -tls_ca etcd-serving-ip-10-0-128-94.us-west-1.compute.internal.crt https+insecure://127.0.0.1:2379/debug/pprof/profile

and it works, yet there seems to be some issues with certificates:

```
Fetching profile over HTTP from https+insecure://127.0.0.1:2379/debug/pprof/profile
https+insecure://127.0.0.1:2379/debug/pprof/profile: Get "https://127.0.0.1:2379/debug/pprof/profile": remote error: tls: bad certificate
failed to fetch any source profiles
```

While the equivalent curl command works just fine:

> curl -k --key etcd-serving-ip-10-0-128-94.us-west-1.compute.internal.key --cert etcd-serving-ip-10-0-128-94.us-west-1.compute.internal.crt https://127.0.0.1:2379/debug/pprof/profile -o some_file.ext

For the time being, you need to work with the indirection of outputting to a local file like above (`-o some_file.ext`) and run `go tool` via:

> go tool pprof -http localhost:8080 some_file.ext

which opens a browser with the given profile data.
