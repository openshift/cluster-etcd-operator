package extended

import (
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
)

var _ = g.Describe("[Jira:etcd][sig-etcd] sanity test", func() {
	g.It("should always pass [Suite:openshift/cluster-etcd-operator/conformance/parallel]", func() {
		o.Expect(true).To(o.BeTrue())
	})
})
