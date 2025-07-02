// Code generated for package assets by go-bindata DO NOT EDIT. (@generated)
// sources:
// bindata/tnfdeployment/clusterrole-binding.yaml
// bindata/tnfdeployment/clusterrole.yaml
// bindata/tnfdeployment/job.yaml
// bindata/tnfdeployment/role-binding.yaml
// bindata/tnfdeployment/role.yaml
// bindata/tnfdeployment/sa.yaml
package assets

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
    app.kubernetes.io/name: tnf-setup
  name: tnf-setup-clusterrole-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: tnf-setup-clusterrole
subjects:
  - kind: ServiceAccount
    name: tnf-setup-manager
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
  labels:
    app.kubernetes.io/name: tnf-setup
  name: tnf-setup-clusterrole
rules:
  - apiGroups:
      - ""
    resources:
      - nodes
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
      - config.openshift.io
    resources:
      - clusterversions
    verbs:
      - get
      - list
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

var _tnfdeploymentJobYaml = []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    app.kubernetes.io/name: <injected>
  namespace: openshift-etcd
  name: <injected>
spec:
  template:
    metadata:
      annotations:
        openshift.io/required-scc: "privileged"
    spec:
      containers:
        - name: tnf-job
          image: <injected>
          imagePullPolicy: IfNotPresent
          command: [ "tnf-setup-runner", "<injected>" ]
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
          securityContext:
            privileged: true
            allowPrivilegeEscalation: true
      hostIPC: false
      hostNetwork: false
      hostPID: true
      priorityClassName: system-node-critical
      serviceAccountName: tnf-setup-manager
      terminationGracePeriodSeconds: 10
      restartPolicy: Never
  backoffLimit: 3
`)

func tnfdeploymentJobYamlBytes() ([]byte, error) {
	return _tnfdeploymentJobYaml, nil
}

func tnfdeploymentJobYaml() (*asset, error) {
	bytes, err := tnfdeploymentJobYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/job.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _tnfdeploymentRoleBindingYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  labels:
    app.kubernetes.io/name: tnf-setup
  namespace: openshift-etcd
  name: tnf-setup-role-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: tnf-setup-role
subjects:
  - kind: ServiceAccount
    name: tnf-setup-manager
`)

func tnfdeploymentRoleBindingYamlBytes() ([]byte, error) {
	return _tnfdeploymentRoleBindingYaml, nil
}

func tnfdeploymentRoleBindingYaml() (*asset, error) {
	bytes, err := tnfdeploymentRoleBindingYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/role-binding.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _tnfdeploymentRoleYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  labels:
    app.kubernetes.io/name: tnf-setup
  namespace: openshift-etcd
  name: tnf-setup-role
rules:
  - apiGroups:
      - ""
    resources:
      - events
    verbs:
      - create
      - patch
  - apiGroups:
      - ""
    resources:
      - secrets
    verbs:
      - get
      - list
      - watch
  - apiGroups:
      - batch
    resources:
      - jobs
    verbs:
      - get
      - list
      - watch
`)

func tnfdeploymentRoleYamlBytes() ([]byte, error) {
	return _tnfdeploymentRoleYaml, nil
}

func tnfdeploymentRoleYaml() (*asset, error) {
	bytes, err := tnfdeploymentRoleYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "tnfdeployment/role.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _tnfdeploymentSaYaml = []byte(`apiVersion: v1
kind: ServiceAccount
metadata:
  labels:
    app.kubernetes.io/name: tnf-setup
  namespace: openshift-etcd
  name: tnf-setup-manager
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
	"tnfdeployment/clusterrole-binding.yaml": tnfdeploymentClusterroleBindingYaml,
	"tnfdeployment/clusterrole.yaml":         tnfdeploymentClusterroleYaml,
	"tnfdeployment/job.yaml":                 tnfdeploymentJobYaml,
	"tnfdeployment/role-binding.yaml":        tnfdeploymentRoleBindingYaml,
	"tnfdeployment/role.yaml":                tnfdeploymentRoleYaml,
	"tnfdeployment/sa.yaml":                  tnfdeploymentSaYaml,
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
		"clusterrole-binding.yaml": {tnfdeploymentClusterroleBindingYaml, map[string]*bintree{}},
		"clusterrole.yaml":         {tnfdeploymentClusterroleYaml, map[string]*bintree{}},
		"job.yaml":                 {tnfdeploymentJobYaml, map[string]*bintree{}},
		"role-binding.yaml":        {tnfdeploymentRoleBindingYaml, map[string]*bintree{}},
		"role.yaml":                {tnfdeploymentRoleYaml, map[string]*bintree{}},
		"sa.yaml":                  {tnfdeploymentSaYaml, map[string]*bintree{}},
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
