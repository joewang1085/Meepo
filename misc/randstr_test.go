package misc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRandString(t *testing.T) {
	zero := RandString(0)
	assert.Equal(t, "", zero)

	rand1 := RandString(1)
	assert.Equal(t, 1, len(rand1))

	rand100 := RandString(100)
	assert.Equal(t, 100, len(rand100))

	rand6_1 := RandString(6)
	rand6_2 := RandString(6)
	assert.NotEqual(t, rand6_1, rand6_2)
}
