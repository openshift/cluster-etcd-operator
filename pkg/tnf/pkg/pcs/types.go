package pcs

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

// StonithConfig represents the minimum part of the configuration of stonith devices.
type StonithConfig struct {
	Primitives []Primitive `json:"primitives"`
}

type Primitive struct {
	Id                 string               `json:"id"`
	AgentName          AgentName            `json:"agent_name"`
	InstanceAttributes []InstanceAttributes `json:"instance_attributes"`
}

type AgentName struct {
	Type string `json:"type"`
}

type InstanceAttributes struct {
	Id      string   `json:"id"`
	NvPairs []NvPair `json:"nvpairs"`
}

type NvPair struct {
	Id    string `json:"id"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

func GetStonithConfig(ctx context.Context) (StonithConfig, error) {
	klog.Info("Getting stonith config")
	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs stonith config --output-format json")
	// stderr contains a warning!
	// don't log stdout, might contain passwords!
	if err != nil {
		klog.Error(err, "Failed to get stonith config", "stderr", stdErr)
		return StonithConfig{}, fmt.Errorf("failed to get stonith config: %v", err)
	}
	stonithConfig, err := unmarshalStonithConfig(stdOut)
	if err != nil {
		klog.Error(err, "Failed to unmarshal stonith config")
		return stonithConfig, fmt.Errorf("failed to unmarshal stonith config: %v", err)
	}
	return stonithConfig, nil
}

func unmarshalStonithConfig(in string) (StonithConfig, error) {
	var stonithConfig StonithConfig
	err := json.Unmarshal([]byte(in), &stonithConfig)
	return stonithConfig, err
}
