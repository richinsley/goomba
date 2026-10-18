package goomba

import "golang.org/x/sys/unix"

// oNoatime is O_NOATIME, which keeps reading a directory from updating its
// access time.
const oNoatime = unix.O_NOATIME
