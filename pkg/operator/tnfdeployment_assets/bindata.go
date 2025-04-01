// Code generated for package tnfdeployment_assets by go-bindata DO NOT EDIT. (@generated)
// sources:
// bindata/tnfdeployment/clusterrole-binding.yaml
// bindata/tnfdeployment/clusterrole.yaml
// bindata/tnfdeployment/deployment.yaml
// bindata/tnfdeployment/leaderelection-role.yaml
// bindata/tnfdeployment/leaderelection-rolebinding.yaml
// bindata/tnfdeployment/sa.yaml
package tnfdeployment_assets

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type asset struct {
	bytes []byte
	info  os.FileInfo
}

type bindataFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
}

// Name return file name
func (fi bindataFileInfo) Name() string {
	return fi.name
}

// Size return file size
func (fi bindataFileInfo) Size() int64 {
	return fi.size
}

// Mode return file mode
func (fi bindataFileInfo) Mode() os.FileMode {
	return fi.mode
}

// Mode return file modify time
func (fi bindataFileInfo) ModTime() time.Time {
	return fi.modTime
}

// IsDir return file whether a directory
func (fi bindataFileInfo) IsDir() bool {
	return fi.mode&os.ModeDir != 0
}

// Sys return file is sys mode
func (fi bindataFileInfo) Sys() interface{} {
	return nil
}

var _tnfdeploymentClusterroleBindingYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  labels:
    app.kubernetes.io/name: tnf-operator
  name: tnf-operator-manager-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: tnf-operator-manager-role
subjects:
  - kind: ServiceAccount
    name: tnf-operator-controller-manager
    namespace: openshift-etcd
`)

func tnfdeploymentClusterroleBindingYamlBytes() ([]byte, error) {
	return _tnfdeploymentClusterroleBindingYaml, nil
}

func tnfdeploymentClusterroleBindingYaml() (*asset, error) {
	bytes, err := tnfdeploymentClusterroleBindingYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/clusterrole-binding.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _tnfdeploymentClusterroleYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: tnf-operator-manager-role
rules:
  - apiGroups:
      - apps
    resources:
      - deployments
    verbs:
      - create
      - get
      - list
      - patch
      - update
      - watch
  - apiGroups:
      - config.openshift.io
    resources:
      - clusterversions
      - infrastructures
    verbs:
      - get
      - list
      - watch
  - apiGroups:
      - ""
    resources:
      - nodes
    verbs:
      - get
      - list
      - watch
  - apiGroups:
      - ""
    resources:
      - pods
    verbs:
      - get
      - list
      - watch
  - apiGroups:
      - ""
    resources:
      - secrets
    verbs:
      - get
      - list
      - watch
  - apiGroups:
      - operator.openshift.io
    resources:
      - etcds
    verbs:
      - get
      - list
      - patch
      - update
      - watch
  - apiGroups:
      - security.openshift.io
    resourceNames:
      - privileged
    resources:
      - securitycontextconstraints
    verbs:
      - use
`)

func tnfdeploymentClusterroleYamlBytes() ([]byte, error) {
	return _tnfdeploymentClusterroleYaml, nil
}

func tnfdeploymentClusterroleYaml() (*asset, error) {
	bytes, err := tnfdeploymentClusterroleYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/clusterrole.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _tnfdeploymentDeploymentYaml = []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app.kubernetes.io/name: tnf-operator
    control-plane: tnf-controller-manager
  name: tnf-operator
spec:
  replicas: 1
  selector:
    matchLabels:
      control-plane: tnf-controller-manager
  template:
    metadata:
      annotations:
        kubectl.kubernetes.io/default-container: tnf-controller
        openshift.io/required-scc: "privileged"
      labels:
        control-plane: tnf-controller-manager
    spec:
      containers:
        - name: tnf-controller
          image: quay.io/openshift/origin-cluster-etcd-operator
          imagePullPolicy: Always
          command: ["cluster-etcd-operator", "tnf-setup"]
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 128Mi
          securityContext:
            privileged: true
            allowPrivilegeEscalation: true
      hostIPC: false
      hostNetwork: false
      hostPID: true
      serviceAccountName: tnf-operator-controller-manager
      terminationGracePeriodSeconds: 10
`)

func tnfdeploymentDeploymentYamlBytes() ([]byte, error) {
	return _tnfdeploymentDeploymentYaml, nil
}

func tnfdeploymentDeploymentYaml() (*asset, error) {
	bytes, err := tnfdeploymentDeploymentYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/deployment.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _tnfdeploymentLeaderelectionRoleYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  labels:
    app.kubernetes.io/name: tnf-operator
  name: tnf-operator-leader-election-role
rules:
  - apiGroups:
      - ""
    resources:
      - configmaps
    verbs:
      - get
      - list
      - watch
      - create
      - update
      - patch
      - delete
  - apiGroups:
      - coordination.k8s.io
    resources:
      - leases
    verbs:
      - get
      - list
      - watch
      - create
      - update
      - patch
      - delete
  - apiGroups:
      - ""
    resources:
      - events
    verbs:
      - create
      - patch
`)

func tnfdeploymentLeaderelectionRoleYamlBytes() ([]byte, error) {
	return _tnfdeploymentLeaderelectionRoleYaml, nil
}

func tnfdeploymentLeaderelectionRoleYaml() (*asset, error) {
	bytes, err := tnfdeploymentLeaderelectionRoleYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/leaderelection-role.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _tnfdeploymentLeaderelectionRolebindingYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  labels:
    app.kubernetes.io/name: tnf-operator
  name: tnf-operator-leader-election-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: tnf-operator-leader-election-role
subjects:
  - kind: ServiceAccount
    name: tnf-operator-controller-manager
    namespace: openshift-etcd
`)

func tnfdeploymentLeaderelectionRolebindingYamlBytes() ([]byte, error) {
	return _tnfdeploymentLeaderelectionRolebindingYaml, nil
}

func tnfdeploymentLeaderelectionRolebindingYaml() (*asset, error) {
	bytes, err := tnfdeploymentLeaderelectionRolebindingYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/leaderelection-rolebinding.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _tnfdeploymentSaYaml = []byte(`apiVersion: v1
kind: ServiceAccount
metadata:
  labels:
    app.kubernetes.io/name: tnf-operator
  name: tnf-operator-controller-manager
`)

func tnfdeploymentSaYamlBytes() ([]byte, error) {
	return _tnfdeploymentSaYaml, nil
}

func tnfdeploymentSaYaml() (*asset, error) {
	bytes, err := tnfdeploymentSaYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/sa.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

// Asset loads and returns the asset for the given name.
// It returns an error if the asset could not be found or
// could not be loaded.
func Asset(name string) ([]byte, error) {
	cannonicalName := strings.Replace(name, "\\", "/", -1)
	if f, ok := _bindata[cannonicalName]; ok {
		a, err := f()
		if err != nil {
			return nil, fmt.Errorf("Asset %s can't read by error: %v", name, err)
		}
		return a.bytes, nil
	}
	return nil, fmt.Errorf("Asset %s not found", name)
}

// MustAsset is like Asset but panics when Asset would return an error.
// It simplifies safe initialization of global variables.
func MustAsset(name string) []byte {
	a, err := Asset(name)
	if err != nil {
		panic("asset: Asset(" + name + "): " + err.Error())
	}

	return a
}

// AssetInfo loads and returns the asset info for the given name.
// It returns an error if the asset could not be found or
// could not be loaded.
func AssetInfo(name string) (os.FileInfo, error) {
	cannonicalName := strings.Replace(name, "\\", "/", -1)
	if f, ok := _bindata[cannonicalName]; ok {
		a, err := f()
		if err != nil {
			return nil, fmt.Errorf("AssetInfo %s can't read by error: %v", name, err)
		}
		return a.info, nil
	}
	return nil, fmt.Errorf("AssetInfo %s not found", name)
}

// AssetNames returns the names of the assets.
func AssetNames() []string {
	names := make([]string, 0, len(_bindata))
	for name := range _bindata {
		names = append(names, name)
	}
	return names
}

// _bindata is a table, holding each asset generator, mapped to its name.
var _bindata = map[string]func() (*asset, error){
	"tnfdeployment/clusterrole-binding.yaml":        tnfdeploymentClusterroleBindingYaml,
	"tnfdeployment/clusterrole.yaml":                tnfdeploymentClusterroleYaml,
	"tnfdeployment/deployment.yaml":                 tnfdeploymentDeploymentYaml,
	"tnfdeployment/leaderelection-role.yaml":        tnfdeploymentLeaderelectionRoleYaml,
	"tnfdeployment/leaderelection-rolebinding.yaml": tnfdeploymentLeaderelectionRolebindingYaml,
	"tnfdeployment/sa.yaml":                         tnfdeploymentSaYaml,
}

// AssetDir returns the file names below a certain
// directory embedded in the file by go-bindata.
// For example if you run go-bindata on data/... and data contains the
// following hierarchy:
//
//	data/
//	  foo.txt
//	  img/
//	    a.png
//	    b.png
//
// then AssetDir("data") would return []string{"foo.txt", "img"}
// AssetDir("data/img") would return []string{"a.png", "b.png"}
// AssetDir("foo.txt") and AssetDir("notexist") would return an error
// AssetDir("") will return []string{"data"}.
func AssetDir(name string) ([]string, error) {
	node := _bintree
	if len(name) != 0 {
		cannonicalName := strings.Replace(name, "\\", "/", -1)
		pathList := strings.Split(cannonicalName, "/")
		for _, p := range pathList {
			node = node.Children[p]
			if node == nil {
				return nil, fmt.Errorf("Asset %s not found", name)
			}
		}
	}
	if node.Func != nil {
		return nil, fmt.Errorf("Asset %s not found", name)
	}
	rv := make([]string, 0, len(node.Children))
	for childName := range node.Children {
		rv = append(rv, childName)
	}
	return rv, nil
}

type bintree struct {
	Func     func() (*asset, error)
	Children map[string]*bintree
}

var _bintree = &bintree{nil, map[string]*bintree{
	"tnfdeployment": {nil, map[string]*bintree{
		"clusterrole-binding.yaml":        {tnfdeploymentClusterroleBindingYaml, map[string]*bintree{}},
		"clusterrole.yaml":                {tnfdeploymentClusterroleYaml, map[string]*bintree{}},
		"deployment.yaml":                 {tnfdeploymentDeploymentYaml, map[string]*bintree{}},
		"leaderelection-role.yaml":        {tnfdeploymentLeaderelectionRoleYaml, map[string]*bintree{}},
		"leaderelection-rolebinding.yaml": {tnfdeploymentLeaderelectionRolebindingYaml, map[string]*bintree{}},
		"sa.yaml":                         {tnfdeploymentSaYaml, map[string]*bintree{}},
	}},
}}

// RestoreAsset restores an asset under the given directory
func RestoreAsset(dir, name string) error {
	data, err := Asset(name)
	if err != nil {
		return err
	}
	info, err := AssetInfo(name)
	if err != nil {
		return err
	}
	err = os.MkdirAll(_filePath(dir, filepath.Dir(name)), os.FileMode(0755))
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(_filePath(dir, name), data, info.Mode())
	if err != nil {
		return err
	}
	err = os.Chtimes(_filePath(dir, name), info.ModTime(), info.ModTime())
	if err != nil {
		return err
	}
	return nil
}

// RestoreAssets restores an asset under the given directory recursively
func RestoreAssets(dir, name string) error {
	children, err := AssetDir(name)
	// File
	if err != nil {
		return RestoreAsset(dir, name)
	}
	// Dir
	for _, child := range children {
		err = RestoreAssets(dir, filepath.Join(name, child))
		if err != nil {
			return err
		}
	}
	return nil
}

func _filePath(dir, name string) string {
	cannonicalName := strings.Replace(name, "\\", "/", -1)
	return filepath.Join(append([]string{dir}, strings.Split(cannonicalName, "/")...)...)
}
