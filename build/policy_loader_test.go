package build

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestMemoizedPolicyFSRefCountedClose(t *testing.T) {
	var initCalls int
	var closeCalls int

	m := &memoizedPolicyFS{
		init: func() (fs.StatFS, func() error, error) {
			initCalls++
			root := fstest.MapFS{
				"policy.rego": &fstest.MapFile{Data: []byte("package docker\n")},
			}
			return root, func() error {
				closeCalls++
				return nil
			}, nil
		},
	}

	first, err := m.get()
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, 1, initCalls)

	second, err := m.get()
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, 1, initCalls)

	require.NoError(t, m.close())
	require.Equal(t, 0, closeCalls)

	third, err := m.get()
	require.NoError(t, err)
	require.NotNil(t, third)
	require.Equal(t, 1, initCalls)

	require.NoError(t, m.close())
	require.Equal(t, 0, closeCalls)

	require.NoError(t, m.close())
	require.Equal(t, 1, closeCalls)
}

func TestMemoizedPolicyFSReinitializesAfterAllRefsClosed(t *testing.T) {
	var initCalls int
	var closeCalls int

	m := &memoizedPolicyFS{
		init: func() (fs.StatFS, func() error, error) {
			initCalls++
			root := fstest.MapFS{
				"policy.rego": &fstest.MapFile{Data: []byte("package docker\n")},
			}
			return root, func() error {
				closeCalls++
				return nil
			}, nil
		},
	}

	first, err := m.get()
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, 1, initCalls)

	require.NoError(t, m.close())
	require.Equal(t, 1, closeCalls)

	second, err := m.get()
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, 2, initCalls)

	require.NoError(t, m.close())
	require.Equal(t, 2, closeCalls)
}
