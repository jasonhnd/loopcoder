package supervisedexec

import "time"

type processActivityObservation struct {
	available bool
	signature string
}

func (o processActivityObservation) changedFrom(prev processActivityObservation) bool {
	return o.available && (!prev.available || o.signature != prev.signature)
}

func processPollInterval(timeout, logInterval time.Duration) time.Duration {
	interval := timeout / 4
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	if interval < logInterval {
		interval = logInterval
	}
	return interval
}
