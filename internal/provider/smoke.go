// Package provider exposes provider runtime smoke compatibility helpers.
package provider

import "github.com/jasonhnd/loopcoder/internal/runtimecap"

func SmokeMatrix(contract runtimecap.Contract) []runtimecap.CompatibilityEntry {
	if len(contract.Providers) == 0 && len(contract.Hosts) == 0 {
		contract = runtimecap.DefaultContract()
	}
	return contract.SmokeMatrix()
}

func Check(contract runtimecap.Contract, providerName, hostName string, role runtimecap.CompatibilityRole) runtimecap.CompatibilityEntry {
	if len(contract.Providers) == 0 && len(contract.Hosts) == 0 {
		contract = runtimecap.DefaultContract()
	}
	return contract.EvaluateCompatibility(providerName, hostName, role)
}
