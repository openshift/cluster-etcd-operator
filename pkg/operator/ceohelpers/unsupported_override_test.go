package ceohelpers

import (
	"k8s.io/apimachinery/pkg/runtime"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
)

func TestIsUnsupportedUnsafeEtcd(t *testing.T) {
	type args struct {
		spec *operatorv1.StaticPodOperatorSpec
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "test value with a bool",
			args: args{spec: &operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{
						Raw: []byte("useUnsupportedUnsafeNonHANonProductionUnstableEtcd: true"),
					},
				},
			}},
			want:    true,
			wantErr: false,
		},
		{
			name: "test value=true with a string",
			args: args{spec: &operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{
						Raw: []byte(`useUnsupportedUnsafeNonHANonProductionUnstableEtcd: "true"`),
					},
				},
			}},
			want:    true,
			wantErr: false,
		},
		{
			name: "test value=false with a string",
			args: args{spec: &operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{
						Raw: []byte(`useUnsupportedUnsafeNonHANonProductionUnstableEtcd: "false"`),
					},
				},
			}},
			want:    false,
			wantErr: false,
		},
		{
			name: "test value=true with a json bool",
			args: args{spec: &operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{
						Raw: []byte(`{"useUnsupportedUnsafeNonHANonProductionUnstableEtcd": true}`),
					},
				},
			}},
			want:    true,
			wantErr: false,
		},
		{
			name: "test value=true with a json string",
			args: args{spec: &operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{
						Raw: []byte(`{"useUnsupportedUnsafeNonHANonProductionUnstableEtcd": "true"}`),
					},
				},
			}},
			want:    true,
			wantErr: false,
		},
		{
			name: "test value=false with a json string",
			args: args{spec: &operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{
						Raw: []byte(`{"useUnsupportedUnsafeNonHANonProductionUnstableEtcd": "false"}`),
					},
				},
			}},
			want:    false,
			wantErr: false,
		},
		{
			name: "test value=false with a json string",
			args: args{spec: &operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{
						Raw: []byte(`{"useUnsupportedUnsafeNonHANonProductionUnstableEtcd": "randomValue"}`),
					},
				},
			}},
			want:    false,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsUnsupportedUnsafeEtcd(tt.args.spec)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsUnsupportedUnsafeEtcd() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("IsUnsupportedUnsafeEtcd() got = %v, want %v", got, tt.want)
			}
		})
	}
}
