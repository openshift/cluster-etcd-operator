package hwspeedhelpers

import (
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/stretchr/testify/require"
)

func TestHardwareSpeedToEnvMap(t *testing.T) {
	tests := []struct {
		name     string
		speed    operatorv1.ControlPlaneHardwareSpeed
		wantEnvs map[string]string
		wantErr  bool
	}{
		{
			name:     "standard",
			speed:    operatorv1.StandardHardwareSpeed,
			wantEnvs: StandardHardwareSpeed(),
		},
		{
			name:     "slower",
			speed:    operatorv1.SlowerHardwareSpeed,
			wantEnvs: SlowerHardwareSpeed(),
		},
		{
			name:    "invalid",
			speed:   operatorv1.ControlPlaneHardwareSpeed("foo"),
			wantErr: true,
		},
		{
			name:    "empty",
			speed:   operatorv1.ControlPlaneHardwareSpeed(""),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotEnvs, err := HardwareSpeedToEnvMap(tt.speed)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.wantEnvs, gotEnvs)
			}
		})
	}
}
