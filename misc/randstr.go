package misc

import (
	"math/rand"
	"time"
)

var alphabet = []rune("1234567890abcdefghijklmnopqrstuvwxyz")
var randSrc = rand.NewSource(time.Now().UnixNano())
var privRand = rand.New(randSrc)

func RandString(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = alphabet[privRand.Intn(len(alphabet))]
	}
	return string(b)
}
