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
		CacheDevice:    "/dev/sdf",
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

	if strings.Contains(u, "[Install]") {
		t.Error("the daemon must not be enabled: a warm-pool boot would start it")
	}

	for _, want := range []string{"MemoryHigh=85%", "MemoryMax=92%", "CPUWeight=50", "User=buildkit", "--net=host", "Requires=buildkit-imds-guard.service"} {
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

func TestModuleStartsOnlyInServiceAndRenders(t *testing.T) {
	c, err := module.Collect(buildkit.New(config()))
	if err != nil {
		t.Fatal(err)
	}

	if len(c.InService) != 1 || c.InService[0].Order != buildkit.StartOrder || !strings.Contains(c.InService[0].Script, "systemctl start buildkitd.service") {
		t.Errorf("in-service %+v", c.InService)
	}

	if len(c.ReadyChecks) != 1 || c.ReadyChecks[0].TimeoutSeconds != 600 || !strings.Contains(c.ReadyChecks[0].Script, "debug workers") {
		t.Errorf("ready %+v", c.ReadyChecks)
	}

	p, err := bootstrap.New(bootstrap.Options{Lifecycle: &bootstrap.Lifecycle{ASGName: "b", HookName: "b-launch"}})
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
		"buildkit-v0.33.0.linux-arm64.tar.gz", "rootlesskit-aarch64.tar.gz", "mkfs.xfs -q -L bkcache", "user.max_user_namespaces = 63359",
		"systemctl enable --now 'buildkit-imds-guard.service'"} {
		if !strings.Contains(r.CloudConfig, want) {
			t.Errorf("user-data lacks %s", want)
		}
	}

	if strings.Contains(r.CloudConfig, "enable 'buildkitd.service'") {
		t.Error("buildkitd is enabled")
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
		"device":      func(c *buildkit.Config) { c.CacheDevice = "/dev/nvme1n1" },
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

// mkfs.xfs refuses a label of more than 12 characters, and the boot script
// then never completes the launch hook.
func TestCacheLabelFitsXFS(t *testing.T) {
	if n := len(buildkit.CacheLabel); n > 12 {
		t.Errorf("CacheLabel %q is %d characters; xfs allows 12", buildkit.CacheLabel, n)
	}
}

func TestOnDemandHasNoLifecycle(t *testing.T) {
	c := config()
	c.OnDemand = true
	c.CacheDevice = ""
	c.BootUnits = []string{"buildkit-cert.service"}

	got, err := buildkit.New(c).Contribute()
	if err != nil {
		t.Fatal(err)
	}

	if len(got.InService)+len(got.ReadyChecks) != 0 {
		t.Errorf("an on-demand daemon completes no launch hook: %v %v", got.InService, got.ReadyChecks)
	}

	unit := c.UnitText()
	for _, want := range []string{"WantedBy=multi-user.target", "Requires=buildkit-imds-guard.service buildkit-cert.service", "ExecStartPre=+/usr/bin/chown"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}

	if strings.Contains(unit, "RequiresMountsFor") {
		t.Error("the data volume module orders the mount, not RequiresMountsFor")
	}

	// The group mode is untouched.
	g := config()
	if strings.Contains(g.UnitText(), "[Install]") {
		t.Error("the group's unit gained an [Install] section")
	}
}
