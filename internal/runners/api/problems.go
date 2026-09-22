package api

import "nerdola.dev/x/paula/internal/config"

// Problems is what is wrong with a section, collected so all of it is reported
// at once. It is the file's own, since a runner reports what it read the way
// everything else that reads the file does.
type Problems = config.Problems
