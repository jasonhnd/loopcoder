package acceptharness

// FailurePoint names an injectable fault for scenarios.
type FailurePoint string

const (
	FailNone         FailurePoint = ""
	FailPushTimeout  FailurePoint = "push_timeout"
	FailUIDisconnect FailurePoint = "ui_disconnect"
	FailProviderExit FailurePoint = "provider_nonzero"
	FailProviderHang FailurePoint = "provider_hang"
	FailDuplicatePR  FailurePoint = "duplicate_pr"
	FailDuplicateAck FailurePoint = "duplicate_ack"
)

// FailurePlan describes which fault to inject and whether a resume is expected.
type FailurePlan struct {
	Point  FailurePoint
	Resume bool
}
