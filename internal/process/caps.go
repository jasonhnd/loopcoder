//go:build windows

package process

import lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"

const livenessHardCapDefault = lcdefaults.ProcessLivenessCommandCap

var livenessHardCap = livenessHardCapDefault
