package ceohelpers

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	operatorv1 "github.com/openshift/api/operator/v1"
)

func TestIsUnsupportedOptions(t *testing.T) {
	options := []string{"useUnsupportedUnsafeNonHANonProductionUnstableEtcd", "useUnsupportedUnsafeEtcdContainerRemoval"}
	tests := []struct {
		name    string
		args    string
		want    bool
		wantErr bool
	}{
		{
			name:    "test value with a bool",
			args:    "%s: true",
			want:    true,
			wantErr: false,
		},
		{
			name:    "test value=true with a string",
			args:    `%s: "true"`,
			want:    true,
			wantErr: false,
		},
		{
			name:    "test value=false with a string",
			args:    `%s: "false"`,
			want:    false,
			wantErr: false,
		},
		{
			name:    "test value=true with a json bool",
			args:    `{"%s": true}`,
			want:    true,
			wantErr: false,
		},
		{
			name:    "test value=true with a json string",
			args:    `{"%s": "true"}`,
			want:    true,
			wantErr: false,
		},
		{
			name:    "test value=false with a json string",
			args:    `{"%s": "false"}`,
			want:    false,
			wantErr: false,
		},
		{
			name:    "test value=false with a json string",
			args:    `{"%s": "randomValue"}`,
			want:    false,
			wantErr: true,
		},
	}

	for _, option := range options {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s/%s", option, tt.name), func(t *testing.T) {
				spec := &operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						UnsupportedConfigOverrides: runtime.RawExtension{
							Raw: []byte(fmt.Sprintf(tt.args, option)),
						},
					},
				}
				got, err := tryGetUnsupportedValue(spec, option)
				require.Equal(t, tt.wantErr, err != nil)
				require.Equal(t, tt.want, got)
			})
		}
	}
}
