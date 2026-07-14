package progresshost

import (
	"os"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/hostprofile"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

type originRequestFunc func(hostprofile.OriginOptions) (runtimecap.HostRunOriginBindingRequest, bool)

type adapter struct {
	profile string
	origin  originRequestFunc
}

var adapters = []adapter{
	{profile: "codex-cli", origin: hostprofile.CodexOriginBindingRequest},
	{profile: "claude-code", origin: hostprofile.ClaudeOriginBindingRequest},
	{profile: "paseo-style", origin: hostprofile.PaseoOriginBindingRequest},
}

func CurrentObligationFactory() progress.DeliveryObligationFunc {
	return ObligationFactory(os.Getenv)
}

func ObligationFactory(getenv func(string) string) progress.DeliveryObligationFunc {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return func(receipt progress.ProgressReceipt) (progress.DeliveryObligation, bool) {
		binding, ok := currentBinding(hostprofile.OriginOptions{
			ProjectID:     receipt.ProjectID,
			DeliveryRunID: receipt.DeliveryRunID,
			CorrelationID: receipt.CorrelationID,
			Getenv:        getenv,
		})
		if ok && binding.Bound && strings.TrimSpace(binding.BindingID) != "" && strings.TrimSpace(binding.OriginRef) != "" {
			return progress.DeliveryObligation{
				ProjectID:         receipt.ProjectID,
				DeliveryRunID:     receipt.DeliveryRunID,
				ProgressReceiptID: receipt.ProgressReceiptID,
				OriginKind:        "host-run-origin",
				OriginID:          binding.OriginRef,
				SinkKind:          "host",
				SinkID:            binding.BindingID,
				TransportContract: runtimecap.HostProgressKnownOriginReplay,
				AckPolicy:         progress.DeliveryAckPolicyNone,
			}, true
		}
		return progress.DeliveryObligation{
			ProjectID:         receipt.ProjectID,
			DeliveryRunID:     receipt.DeliveryRunID,
			ProgressReceiptID: receipt.ProgressReceiptID,
			OriginKind:        "progress-receipt",
			OriginID:          receipt.CorrelationID,
			SinkKind:          "host",
			SinkID:            receipt.CorrelationID,
			TransportContract: runtimecap.HostProgressNextInvocationReplay,
			AckPolicy:         progress.DeliveryAckPolicyNone,
		}, true
	}
}

func currentBinding(opts hostprofile.OriginOptions) (runtimecap.HostRunOriginBinding, bool) {
	if raw := strings.TrimSpace(opts.Getenv(hostprofile.EnvName)); raw != "" {
		name, ok := hostprofile.NormalizeName(raw)
		if !ok {
			return runtimecap.HostRunOriginBinding{}, false
		}
		return bindingForProfile(name, opts)
	}
	for _, candidate := range adapters {
		if binding, ok := bindingForAdapter(candidate, opts); ok {
			return binding, true
		}
	}
	return runtimecap.HostRunOriginBinding{}, false
}

func bindingForProfile(profile string, opts hostprofile.OriginOptions) (runtimecap.HostRunOriginBinding, bool) {
	for _, candidate := range adapters {
		if candidate.profile == profile {
			return bindingForAdapter(candidate, opts)
		}
	}
	return runtimecap.HostRunOriginBinding{}, false
}

func bindingForAdapter(adapter adapter, opts hostprofile.OriginOptions) (runtimecap.HostRunOriginBinding, bool) {
	req, ok := adapter.origin(opts)
	if !ok {
		return runtimecap.HostRunOriginBinding{}, false
	}
	binding := runtimecap.BindHostRunOrigin(req)
	return binding, binding.Bound
}
