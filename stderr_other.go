//go:build !unix && !windows

package tracepanel

import (
	"errors"
	"os"
)

// swapStderr can't: the profile runs at Go's rate.
func swapStderr(*os.File) (func(), error) { return nil, errors.ErrUnsupported }
