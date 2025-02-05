package management

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilterValidation(t *testing.T) {
	t.Run("Counter", func(t *testing.T) {
		for _, f := range []StatusFilter{Pending, InProgress, Stale, Completed, Retrying, All} {
			assert.Nil(t, f.Validate())
		}
	})
	t.Run("Runtime", func(t *testing.T) {
		for _, f := range []RuntimeFilter{Duration, Latency, Running} {
			assert.Nil(t, f.Validate())
		}
	})
	t.Run("Error", func(t *testing.T) {
		for _, f := range []ErrorFilter{UniqueErrors, AllErrors, StatsOnly} {
			assert.Nil(t, f.Validate())
		}
	})

	t.Run("InvalidValues", func(t *testing.T) {
		for _, f := range []string{"", "foo", "bleh", "0"} {
			assert.Error(t, StatusFilter(f).Validate())
			assert.Error(t, RuntimeFilter(f).Validate())
			assert.Error(t, ErrorFilter(f).Validate())
		}
	})
}
