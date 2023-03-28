package health

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeAliveness struct {
	result bool
}

func (f fakeAliveness) Alive() bool {
	return f.result
}

func TestAlivenessHappyPath(t *testing.T) {
	mac := NewMultiAlivenessChecker()
	require.True(t, mac.Alive())

	mac.Add("a", fakeAliveness{result: true})
	mac.Add("b", fakeAliveness{result: true})
	require.True(t, mac.Alive())

	mac.Add("b", fakeAliveness{result: false})
	require.False(t, mac.Alive())
}
