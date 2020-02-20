package options

// ManifestConfig is a struct of values to be used in manifest templates.
type ManifestConfig struct {
	// ConfigHostPath is a host path mounted into the controller manager pods to hold the config file.
	ConfigHostPath string

	// ConfigFileName is the filename of config file inside ConfigHostPath.
	ConfigFileName string

	// CloudProviderHostPath is a host path mounted into the apiserver pods to hold cloud provider configuration.
	CloudProviderHostPath string

	// SecretsHostPath holds certs and keys
	SecretsHostPath string

	// Namespace is the target namespace for the bootstrap controller manager to be created.
	Namespace string

	// Images are  pull spec of the images to use for the controller manager.
	Images

	// EtcdAddress are addresses used to populate etcd static pod spec.
	EtcdAddress

	// ImagePullPolicy specifies the image pull policy to use for the images.
	ImagePullPolicy string
}

// FileConfig
type FileConfig struct {
	// BootstrapConfig holds the rendered control plane component config file for bootstrapping (phase 1).
	BootstrapConfig []byte

	// PostBootstrapConfig holds the rendered control plane component config file after bootstrapping (phase 2).
	PostBootstrapConfig []byte

	// Assets holds the loaded assets like certs and keys.
	Assets map[string][]byte
}

type Images struct {
	Etcd            string
	SetupEtcdEnv    string
	KubeClientAgent string
}

type EtcdAddress struct {
	ListenClient       string
	ListenPeer         string
	ListenMetricServer string
	ListenMetricProxy  string
	LocalHost          string
	BootstrapIP        string
}

type TemplateData struct {
	ManifestConfig
	FileConfig
}
