package render

import (
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func runRender(args ...string) (*cobra.Command, error) {
	errOut := &bytes.Buffer{}
	c := NewRenderCommand(errOut)
	os.Args = append([]string{"render.test"}, args...)
	if err := c.Execute(); err != nil {
		panic(err)
	}
	errBytes, err := ioutil.ReadAll(errOut)
	if err != nil {
		panic(err)
	}
	if len(errBytes) == 0 {
		return c, nil
	}
	return c, errors.New(string(errBytes))
}

func setupAssetOutputDir(testName string) (teardown func(), outputDir string, err error) {
	outputDir, err = ioutil.TempDir("", testName)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "manifests"), os.ModePerm); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "configs"), os.ModePerm); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "static-pod-resources", "etcd-member"), os.ModePerm); err != nil {
		return nil, "", err
	}

	teardown = func() {
		os.RemoveAll(outputDir)
	}
	return
}

func setOutputFlags(args []string, dir string) []string {
	newArgs := []string{}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--asset-output-dir=") {
			newArgs = append(newArgs, "--asset-output-dir="+filepath.Join(dir, "manifests"))
			continue
		}
		if strings.HasPrefix(arg, "--config-output-file=") {
			newArgs = append(newArgs, "--config-output-file="+filepath.Join(dir, "configs", "config.yaml"))
			continue
		}
		if strings.HasPrefix(arg, "--etcd-static-resources-dir=") {
			newArgs = append(newArgs, "--etcd-static-resources-dir="+filepath.Join(dir, "static-pod-resources", "etcd-member"))
			continue
		}

		newArgs = append(newArgs, arg)
	}
	return newArgs
}

func TestRenderCommand(t *testing.T) {
	assetsInputDir := filepath.Join("testdata", "tls")
	templateDir := filepath.Join("..", "..", "..", "bindata", "bootkube")

	tests := []struct {
		name    string
		args    []string
		errFunc func(error)
	}{
		{
			name: "no-flags",
			args: []string{
				"--templates-input-dir=" + templateDir,
			},
			errFunc: func(err error) {
				if err == nil {
					t.Fatalf("expected required flags error")
				}
			},
		},
		{
			name: "happy-path",
			args: []string{
				"--asset-input-dir=" + assetsInputDir,
				"--templates-input-dir=" + templateDir,
				"--asset-output-dir=",
				"--config-output-file=",
				"--etcd-static-resources-dir=",
				"--etcd-ca=" + assetsInputDir + "/etcd-ca-bundle.crt",
				"--etcd-metric-ca=" + assetsInputDir + "/etcd-metric-ca-bundle.crt",
				"--manifest-etcd-image=foo",
				"--manifest-kube-client-agent-image=foo",
				"--manifest-setup-etcd-env-image=foo",
				"--etcd-config-file=foo",
				"--etcd-discovery-domain=foo",
			},
		},
	}

	for _, test := range tests {
		teardown, outputDir, err := setupAssetOutputDir(test.name)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", test.name, err)
		}
		defer teardown()

		test.args = setOutputFlags(test.args, outputDir)

		_, err = runRender(test.args...)
		if err != nil && test.errFunc == nil {
			t.Errorf("%s: got unexpected error %v", test.name, err)
			continue
		}
		if err != nil {
			test.errFunc(err)
			continue
		}
	}
}
