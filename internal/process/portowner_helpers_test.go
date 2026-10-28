package process

import (
	"os"
	"strconv"
)

func selfPID() int      { return os.Getpid() }
func itoa(n int) string { return strconv.Itoa(n) }
