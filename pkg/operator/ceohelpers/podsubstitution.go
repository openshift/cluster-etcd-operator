package ceohelpers

import (
	"math"
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

type ProbeConfig struct {
	TimeoutSeconds      int
	PeriodSeconds       int
	SuccessThreshold    int
	FailureThreshold    int
	InitialDelaySeconds int
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
	ReadinessProbe      ProbeConfig
	LivenessProbe       ProbeConfig
	StartupProbe        ProbeConfig
}

// calculateFailureThreshold calculates the failure threshold based on backend quota size.
// The base configuration is designed for 8GiB, and we scale proportionally for other sizes.
// Formula: scaledThreshold = baseThreshold * (quotaGiB / 8.0), rounded to nearest integer
func calculateFailureThreshold(baseThreshold int, quotaGiB int32) int {
	// always assume 8GB quota as the default baseline for failure thresholds
	if quotaGiB <= 8 {
		quotaGiB = 8
	}

	// Calculate the scaling factor based on quota size relative to 8GB baseline
	scalingFactor := float64(quotaGiB) / 8.0
	scaledThreshold := float64(baseThreshold) * scalingFactor

	// Round to nearest integer, with minimum of 1
	result := int(math.Round(scaledThreshold))
	if result < 1 {
		result = 1
	}

	return result
}

// GetPodSubstitution creates a PodSubstitutionTemplate with values derived from StaticPodOperatorSpec,
// image pull spec and environment variables. It determines whether the Etcd container should be enabled
// based on the operator's configuration.
func GetPodSubstitution(
	operatorSpec *operatorv1.StaticPodOperatorSpec,
	imagePullSpec, operatorImagePullSpec string,
	envVarMap map[string]string,
	etcd *operatorv1.Etcd) (*PodSubstitutionTemplate, error) {

	var nameValues []NameValue
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		nameValues = append(nameValues, NameValue{Name: k, Value: v})
	}

	shouldRemoveEtcdContainer, err := IsUnsupportedUnsafeEtcdContainerRemoval(operatorSpec)
	if err != nil {
		return nil, err
	}

	// Calculate failure thresholds based on etcd.Spec.BackendQuotaGiB
	// Base values are designed for 8GiB backend quota, ~4 failure thresholds for each GiB of quota configured.
	livenessFailureThreshold := calculateFailureThreshold(20, etcd.Spec.BackendQuotaGiB)
	startupFailureThreshold := calculateFailureThreshold(30, etcd.Spec.BackendQuotaGiB)

	defaultReadinessProbeConfig := ProbeConfig{
		TimeoutSeconds:      30,
		PeriodSeconds:       1,
		SuccessThreshold:    1,
		FailureThreshold:    3,
		InitialDelaySeconds: 0,
	}

	// we calculate with 10 gigs to defrag (100s) for additional slack
	// 20*5s=100s, worst case before restarting etcd
	defaultLivenessProbeConfig := ProbeConfig{
		TimeoutSeconds:      30,
		PeriodSeconds:       5,
		SuccessThreshold:    1,
		FailureThreshold:    livenessFailureThreshold,
		InitialDelaySeconds: 0,
	}

	// 30*10s+30s of startup (5.30m total, requires about 23mb/s bandwidth to read 8gigs)
	defaultStartupProbeConfig := ProbeConfig{
		TimeoutSeconds:      30,
		PeriodSeconds:       5,
		SuccessThreshold:    1,
		FailureThreshold:    startupFailureThreshold,
		InitialDelaySeconds: 30,
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
		ReadinessProbe:      defaultReadinessProbeConfig,
		LivenessProbe:       defaultLivenessProbeConfig,
		StartupProbe:        defaultStartupProbeConfig,
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
