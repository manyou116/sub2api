package service

import (
	"log/slog"
	"os"
	"strings"
)

const openAIBoundedProbeEnv = "SUB2API_SCHEDULER_BOUNDED_PROBE"

func boundedSchedulerProbeEnvEnabled() bool {
	flag := strings.TrimSpace(os.Getenv(openAIBoundedProbeEnv))
	return flag == "1" || strings.EqualFold(flag, "true")
}

// Aggregate counters are per gateway. Sample logs make legacy-path fallback
// rates observable without introducing a new metrics API or logging account data.
func (s *OpenAIGatewayService) recordBoundedProbe(fallback bool) {
	if fallback {
		s.legacyBoundedProbeFallbacks.Add(1)
	}
	total := s.legacyBoundedProbeTotal.Add(1)
	if total == 1 || total%1024 == 0 {
		slog.Info("scheduler bounded probe", "attempts", total,
			"fallbacks", s.legacyBoundedProbeFallbacks.Load())
	}
}
