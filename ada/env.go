package ada

import (
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"sync"
)

var (
	hostFactsOnce sync.Once
	hostFactsVal  []string
)

// HostFacts returns durable, observed facts about the host the agent runs on —
// OS, distro, the available package manager (with its non-interactive install
// command), and the current user. This is the research notes' §5.3 "semantic
// memory": without it the model GUESSES the environment (e.g. apt-get on an Arch
// box) and fails every time. Detected once on first call, then cached.
func HostFacts() []string {
	hostFactsOnce.Do(func() { hostFactsVal = detectHostFacts() })
	return hostFactsVal
}

func detectHostFacts() []string {
	f := []string{"os=" + runtime.GOOS, "arch=" + runtime.GOARCH}

	if id := osReleaseID(); id != "" {
		f = append(f, "distro="+id)
	}
	if name, install := detectPackageManager(); name != "" {
		f = append(f, "package_manager="+name+" (install with: "+install+")")
	}
	if u, err := user.Current(); err == nil {
		if os.Geteuid() == 0 {
			f = append(f, "user="+u.Username+" (root — do NOT prefix sudo)")
		} else {
			f = append(f, "user="+u.Username)
		}
	}
	return f
}

// osReleaseID reads ID= from /etc/os-release (e.g. "arch", "ubuntu", "fedora").
func osReleaseID() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "ID="); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// detectPackageManager finds the first available package manager and its
// non-interactive install command. Arch's pacman is probed first (reference
// platform), then the rest.
func detectPackageManager() (name, install string) {
	for _, c := range []struct{ bin, install string }{
		{"pacman", "pacman -S --noconfirm <pkg>"},
		{"apt-get", "apt-get install -y <pkg>"},
		{"dnf", "dnf install -y <pkg>"},
		{"zypper", "zypper install -y <pkg>"},
		{"apk", "apk add <pkg>"},
		{"brew", "brew install <pkg>"},
	} {
		if _, err := exec.LookPath(c.bin); err == nil {
			return c.bin, c.install
		}
	}
	return "", ""
}
