package ceohelpers

import (
	"reflect"
	"strings"
	"text/template"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"k8s.io/apimachinery/pkg/util/sets"
)

type NameValue struct {
	Name  string
	Value string
}

type PodSubstitutionTemplate struct {
	Image               string
	OperatorImage       string
	ListenAddress       string
	LocalhostAddress    string
	LogLevel            string
	EnvVars             []NameValue
	BackupArgs          []string
	CipherSuites        string
	EnableEtcdContainer bool
}

// GetPodSubstitution creates a PodSubstitutionTemplate with values derived from StaticPodOperatorSpec,
// image pull spec and environment variables. It determines whether the Etcd container should be enabled
// based on the operator's configuration.
func GetPodSubstitution(
	operatorSpec *operatorv1.StaticPodOperatorSpec,
	imagePullSpec, operatorImagePullSpec string,
	envVarMap map[string]string) (*PodSubstitutionTemplate, error) {

	var nameValues []NameValue
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		nameValues = append(nameValues, NameValue{Name: k, Value: v})
	}

	shouldRemoveEtcdContainer, err := IsUnsupportedUnsafeEtcdContainerRemoval(operatorSpec)
	if err != nil {
		return nil, err
	}

	return &PodSubstitutionTemplate{
		Image:               imagePullSpec,
		OperatorImage:       operatorImagePullSpec,
		ListenAddress:       "0.0.0.0",   // TODO: this needs updating to detect ipv6-ness
		LocalhostAddress:    "127.0.0.1", // TODO: this needs updating to detect ipv6-ness
		LogLevel:            LoglevelToZap(operatorSpec.LogLevel),
		EnvVars:             nameValues,
		CipherSuites:        envVarMap["ETCD_CIPHER_SUITES"],
		EnableEtcdContainer: !shouldRemoveEtcdContainer,
	}, nil
}

// RenderTemplate renders a Pod template from the Assets with the data from a PodSubstitutionTemplate
func RenderTemplate(templateName string, subs *PodSubstitutionTemplate) (string, error) {
	fm := template.FuncMap{"quote": func(arg reflect.Value) string {
		return "\"" + arg.String() + "\""
	}}
	podBytes := bindata.MustAsset(templateName)
	tmpl, err := template.New("pod").Funcs(fm).Parse(string(podBytes))
	if err != nil {
		return "", err
	}

	w := &strings.Builder{}
	err = tmpl.Execute(w, subs)
	if err != nil {
		return "", err
	}
	return w.String(), nil
}

// LoglevelToZap converts a openshift/api/operator/v1 LogLevel into a corresponding Zap log level string
func LoglevelToZap(logLevel operatorv1.LogLevel) string {
	switch logLevel {
	case operatorv1.Debug, operatorv1.Trace, operatorv1.TraceAll:
		return "debug"
	default:
		return "info"
	}
}
