package e2e_test

import (
	"os"
	"reflect"
	"sort"
	"testing"
	"unsafe"
)

func TestMain(m *testing.M) {
	// TODO: Remove this once scale down is implemented
	reorderTestExecutionOrder(m)
	os.Exit(m.Run())
}

// reorderTestExecutionOrder a hack to place the vertical scaling test at the end of execution chain
// Some test like TestEtcdQuorumGuard assume exactly 3 master cluster. Since the scaling test add new machine(s) and
// we don't have a function to scale them down we need to run the new test after existing ones.
//
// This function should be removed once scaling down is implemented
func reorderTestExecutionOrder(m *testing.M) {
	pointerVal := reflect.ValueOf(m)
	val := reflect.Indirect(pointerVal)

	testsMember := val.FieldByName("tests")
	ptrToTests := unsafe.Pointer(testsMember.UnsafeAddr())
	realPtrToTests := (*[]testing.InternalTest)(ptrToTests)
	tests := *realPtrToTests

	sort.Slice(tests, func(i, j int) bool {
		return tests[i].Name != "TestScalingUpSingleNode"
	})
}
