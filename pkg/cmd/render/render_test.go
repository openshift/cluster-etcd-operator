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
		if strings.HasPrefix(arg, "--etcd-config-dir=") {
			newArgs = append(newArgs, "--etcd-config-dir="+filepath.Join(dir, "etc", "etcd"))
			continue
		}

		newArgs = append(newArgs, arg)
	}
	return newArgs
}

func TestRenderCommand(t *testing.T) {
	assetsInputDir := filepath.Join("testdata", "tls")
	templateDir := filepath.Join("..", "..", "..", "bindata", "bootkube")

	type RenderTest struct {
		name    string
		args    []string
		errFunc func(error)
	}

	tests := []RenderTest{}

	/* Keep these in the same order as render.Validate() so the
	/* coverage can cascade through each */
	required_flags := [][]string{
		[]string{"--asset-input-dir", assetsInputDir},
		[]string{"--asset-output-dir", ""},
		[]string{"--templates-input-dir", templateDir},
		[]string{"--config-output-file", ""},

		[]string{"--etcd-ca", assetsInputDir + "/etcd-ca-bundle.crt"},
		[]string{"--etcd-metric-ca", assetsInputDir + "/etcd-metric-ca-bundle.crt"},
		[]string{"--manifest-etcd-image", "foo"},
		[]string{"--manifest-kube-client-agent-image", "foo"},
		[]string{"--manifest-setup-etcd-env-image", "foo"},
		[]string{"--etcd-discovery-domain", "foo"},
	}

	optional_flags := [][]string{
		[]string{"--etcd-static-resources-dir", ""},
		[]string{"--etcd-config-dir", ""},
	}

	seen_flags := []string{}
	for _, flag := range required_flags {
		tests = append(tests,
			RenderTest{
				name: "missing-flag-" + flag[0],
				args: seen_flags,
				errFunc: func(err error) {
					if err == nil {
						t.Fatalf("expected required flags error")
					}
				},
			},
		)
		seen_flags = append(seen_flags, strings.Join(flag, "="))
	}

	for _, flag := range optional_flags {
		seen_flags = append(seen_flags, strings.Join(flag, "="))
	}

	tests = append(tests,
		RenderTest{
			name: "all-flags",
			args: seen_flags,
		},
	)

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
