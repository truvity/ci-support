package buildkit_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"

	"github.com/truvity/ci-support/pkg/buildkit"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()

	file := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if string(want) != got {
		t.Errorf("%s differs from the golden (go test -update to accept):\n%s", name, got)
	}
}

func config() buildkit.Config {
	return buildkit.Config{
		MaxParallelism: 6,
		TLS:            buildkit.TLS{Cert: "/run/tls/cert.pem", Key: "/run/tls/key.pem", CA: "/run/tls/ca.pem"},
		BootUnits:      []string{"buildkit-cert.service"},
	}
}

func TestTOMLGolden(t *testing.T) {
	s, err := config().TOML()
	if err != nil {
		t.Fatal(err)
	}

	golden(t, "buildkitd.toml", s)

	for _, want := range []string{`max-parallelism = 6`, `"tcp://0.0.0.0:1234"`, `debugAddress = "127.0.0.1:6060"`, `maxUsedSpace = "80GB"`, `rootless = true`} {
		if !strings.Contains(s, want) {
			t.Errorf("toml lacks %s", want)
		}
	}
}

func TestUnitGolden(t *testing.T) {
	u := config().UnitText()
	golden(t, "buildkitd.service", u)

	for _, want := range []string{
		"MemoryHigh=85%", "MemoryMax=92%", "CPUWeight=50", "User=buildkit", "--net=host",
		"WantedBy=multi-user.target", "Requires=buildkit-imds-guard.service buildkit-cert.service", "ExecStartPre=+/usr/bin/chown",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %s", want)
		}
	}
}

func TestGuardDropsMetadataForEveryoneButRoot(t *testing.T) {
	rules := buildkit.GuardRules

	for _, want := range []string{"meta skuid 0 accept", "ip daddr 169.254.169.254 counter drop", "ip6 daddr fd00:ec2::254 counter drop"} {
		if !strings.Contains(rules, want) {
			t.Errorf("guard lacks %s", want)
		}
	}
}

func TestModuleHasNoLifecycleAndRenders(t *testing.T) {
	c, err := module.Collect(buildkit.New(config()))
	if err != nil {
		t.Fatal(err)
	}

	if len(c.InService)+len(c.ReadyChecks) != 0 {
		t.Errorf("a single-instance daemon completes no launch hook: %v %v", c.InService, c.ReadyChecks)
	}

	p, err := bootstrap.New(bootstrap.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.AddTo(p); err != nil {
		t.Fatal(err)
	}

	r, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"buildkit-v0.33.0.linux-arm64.tar.gz", "rootlesskit-aarch64.tar.gz", "user.max_user_namespaces = 63359",
		"systemctl enable --now 'buildkit-imds-guard.service'", "buildkitd.service"} {
		if !strings.Contains(r.CloudConfig, want) {
			t.Errorf("user-data lacks %s", want)
		}
	}

	if strings.Contains(r.CloudConfig, "mkfs.xfs") || strings.Contains(config().UnitText(), "RequiresMountsFor") {
		t.Error("the data volume module owns the mount, not the daemon")
	}
}

func TestRefusals(t *testing.T) {
	for name, mut := range map[string]func(*buildkit.Config){
		"parallelism": func(c *buildkit.Config) { c.MaxParallelism = 0 },
		"port":        func(c *buildkit.Config) { c.Port = 443 },
		"gc":          func(c *buildkit.Config) { c.GC.MaxUsedSpace = "lots" },
		"tls":         func(c *buildkit.Config) { c.TLS.Key = "key.pem" },
		"debug":       func(c *buildkit.Config) { c.DebugAddress = "0.0.0.0:6060" },
		"memory":      func(c *buildkit.Config) { c.MemoryHighPercent, c.MemoryMaxPercent = 95, 90 },
	} {
		c := config()
		mut(&c)

		if _, err := buildkit.New(c).Contribute(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestArch(t *testing.T) {
	for in, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"} {
		if got, err := buildkit.Arch(in); err != nil || got != want {
			t.Errorf("Arch(%s) = %s, %v", in, got, err)
		}
	}

	if _, err := buildkit.Arch("riscv64"); err == nil {
		t.Error("riscv64 accepted")
	}
}

// mkfs.xfs refuses a label of more than 12 characters, and the data volume
// module then never mounts the cache.
func TestCacheLabelFitsXFS(t *testing.T) {
	if n := len(buildkit.CacheLabel); n > 12 {
		t.Errorf("CacheLabel %q is %d characters; xfs allows 12", buildkit.CacheLabel, n)
	}
}
