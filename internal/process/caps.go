package process

import "time"

const livenessHardCapDefault = 5 * time.Second

var livenessHardCap = livenessHardCapDefault
