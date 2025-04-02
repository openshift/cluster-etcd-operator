This directory contains most files which are needed for Two Node With Fencing (TNF)

Additional files are:
- bindata/tnfdeployment: manifests

Modified files for integration of TNF components:
- cmd/cluster-etcd-operator/main.go: added tnf command
- pkg/operator/starter.go: added tnf deployment controller
- Makefile: generate tnf assets