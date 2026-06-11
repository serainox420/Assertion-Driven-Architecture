package ada

import (
	"strings"
	"testing"
)

// The agent was blind to its host (defaulted to apt-get on an Arch box). Host
// facts must at least report the OS and, on a real machine, a package manager.
func TestHostFactsReportOS(t *testing.T) {
	joined := strings.Join(HostFacts(), " ")
	if !strings.Contains(joined, "os=") {
		t.Errorf("host facts should include os=, got %q", joined)
	}
}
