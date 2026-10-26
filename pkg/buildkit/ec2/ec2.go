// Package ec2 runs a BuildKit builder (package buildkit) on an EC2 Auto
// Scaling group with github.com/truvity/asg: one group per architecture,
// behind a listener of a shared internal network load balancer.
//
// What one New call creates:
//
//   - on the shared NLB (asg front.Attach): a TCP listener on the builder's
//     port from the caller's port registry, never TLS (the daemon checks the
//     client certificates itself), a target group of instance targets with a
//     TCP health check, and ingress rules on the NLB's security group that
//     admit only the given sources (the runners' security group) on that
//     port;
//   - the box (asg.New): a launch template (the image of the architecture,
//     IMDSv2 with hop limit 1, an encrypted root and a cache volume), the
//     group with ELB health, a warm pool of stopped instances that takes a
//     scaled-in instance back with its cache (ReuseOnScaleIn), a launch hook
//     that abandons a launch whose daemon never answered, launch-before-
//     terminate instance refresh, the termination policy NewestInstance (the
//     instance with the warm cache stays), the role with exactly the reads
//     its user-data names, and a security group that admits the daemon's
//     port from the NLB's group only.
//
// On the box (the user-data): the daemon (package buildkit), asg's ssmcert
// module (the server certificate, its key and the client CA from SSM into
// /run on every boot into service, and the nightly rotation through
// Standby, since buildkitd never reloads its certificate) and asg's watchdog
// module (`buildctl debug workers` over the unix socket; restarts, then
// marks the instance Unhealthy so the group replaces it).
//
// The one grant that is the builder's own: kms:Decrypt of the certificate
// parameters' key, through SSM, for those parameters only.
package ec2

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/front"
	"github.com/truvity/asg/modules/ssmcert"
	"github.com/truvity/asg/modules/watchdog"

	"github.com/truvity/ci-support/pkg/buildkit"
)

// Defaults.
const (
	DefaultRootVolumeGiB  = 20
	DefaultCacheVolumeGiB = 100
	DefaultCacheDevice    = "/dev/sdf"
	// DefaultIdleTimeoutSeconds is the listener's TCP idle timeout: a build
	// step can be silent for a long time.
	DefaultIdleTimeoutSeconds = 3600
	// DefaultDeregistrationDelaySeconds is how long a leaving instance keeps
	// serving the connections it has.
	DefaultDeregistrationDelaySeconds = 900
	// DefaultHookHeartbeatSeconds bounds a boot into service: the
	// certificate, the daemon and its cache index (minutes on a full cache).
	DefaultHookHeartbeatSeconds = 1800
	// DaemonPort is the daemon's port on the instance.
	DaemonPort = 1234
)

// imageParameters are the public SSM parameters of the current Amazon Linux
// 2023 image per architecture; the image is pinned in the launch template
// at deploy time, so a newer image is a visible diff and a refresh.
var imageParameters = map[string]string{
	"amd64": "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64",
	"arm64": "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64",
}

type (
	// Args configure one builder (one architecture).
	Args struct {
		// Arch is amd64 or arm64. Required.
		Arch string
		// Name is the physical name of the group, its launch template, role
		// and security group prefix (at most 48 characters), such as
		// devel-buildkit-arm64. Default: the logical name.
		Name string
		// InstanceType must be of the architecture. Required.
		InstanceType string
		// Min, Max and Desired size the group. Default 1, 3 and Min. The
		// certificate rotation needs Max above Desired.
		Min, Max, Desired int
		// WarmPoolSize is the warm pool's minimum. Default 1.
		WarmPoolSize *int

		// VPCID and SubnetIDs place the instances (private subnets, several
		// zones). Required.
		VPCID     pulumi.StringInput
		SubnetIDs []pulumi.StringInput
		// PermissionsBoundary is the role's boundary policy ARN.
		PermissionsBoundary string
		// Tags are added to every resource that takes tags.
		Tags map[string]string
		// Provider is the AWS provider for the lookups.
		Provider *aws.Provider

		// NLB is the shared NLB (the file its stack wrote). Required.
		NLB front.SharedNLB
		// Listener names the listener and its target group, such as
		// buildkit-arm64. Default buildkit-<arch>.
		Listener string
		// Port is the listener's port, from the caller's port registry.
		// Required.
		Port int
		// Sources may connect to Port (the runners' security group). At least
		// one.
		Sources []front.Source

		// Certificate are the SSM SecureStrings of the server certificate,
		// its key and the client CA, and the alias of the key they are
		// encrypted with. Required.
		Certificate Certificate

		// Daemon configures buildkitd. TLS and CacheDevice are set here.
		Daemon buildkit.Config
		// CacheVolumeGiB is the cache volume. Default 100.
		CacheVolumeGiB int
		// RootVolumeGiB is the root volume. Default 20.
		RootVolumeGiB int

		// Watchdog tunes the local check. The zero value checks every 30
		// seconds and acts after 3 minutes, with two restarts before the
		// instance is marked Unhealthy.
		Watchdog *watchdog.Args
		// Rotation tunes the certificate rotation (asg ssmcert). Names,
		// parameters, units, port, group and hook are set here.
		Rotation ssmcert.Args
	}

	// Certificate is where the box's TLS material lives.
	Certificate struct {
		// Cert, Key and ClientCA are parameter names, such as
		// /ci/devel/buildkit-server/arm64/cert.
		Cert, Key, ClientCA string
		// KeyAlias is the alias of the customer-managed key of the three
		// parameters, such as alias/devel-ci-buildkit-server. The grant is
		// on that alias (kms:ResourceAliases), so the key need not exist at
		// preview time.
		KeyAlias string
	}

	// Builder is what New creates. It is a plain holder, not a component of
	// its own: the box (truvity:asg:Box) and its front (truvity:asg:Front)
	// are registered with the caller's options, each the parent of its own
	// resources.
	Builder struct {
		// Box is the Auto Scaling group and its parts.
		Box *asg.Appliance
		// Front is the box's listener, target group and rules on the NLB.
		Front *front.Front
		// Rendered is the user-data, for review goldens.
		Rendered *bootstrap.Rendered
	}
)

var (
	aliasRe = regexp.MustCompile(`^alias/[A-Za-z0-9/_-]{1,250}$`)
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
)

func (a Args) withDefaults(name string) Args {
	if a.Name == "" {
		a.Name = name
	}

	if a.Min == 0 && a.Max == 0 && a.Desired == 0 {
		a.Min = 1
	}

	if a.Max == 0 {
		a.Max = 3
	}

	if a.Desired == 0 {
		a.Desired = a.Min
	}

	if a.WarmPoolSize == nil {
		one := 1
		a.WarmPoolSize = &one
	}

	if a.Listener == "" {
		a.Listener = "buildkit-" + a.Arch
	}

	if a.CacheVolumeGiB == 0 {
		a.CacheVolumeGiB = DefaultCacheVolumeGiB
	}

	if a.RootVolumeGiB == 0 {
		a.RootVolumeGiB = DefaultRootVolumeGiB
	}

	return a
}

func (a Args) validate() error {
	var errs []error

	add := func(format string, v ...any) { errs = append(errs, fmt.Errorf(format, v...)) }

	if _, ok := imageParameters[a.Arch]; !ok {
		add("Arch %q: want amd64 or arm64", a.Arch)
	}

	if !nameRe.MatchString(a.Name) {
		add("Name %q: want lower-case letters, digits and -, at most 48", a.Name)
	}

	if a.InstanceType == "" {
		add("InstanceType is required")
	}

	if a.Max <= a.Desired {
		add("Max %d must be above Desired %d: the certificate rotation puts a replacement in service first", a.Max, a.Desired)
	}

	if a.WarmPoolSize != nil && *a.WarmPoolSize < 0 {
		add("WarmPoolSize %d is negative", *a.WarmPoolSize)
	}

	if a.VPCID == nil || len(a.SubnetIDs) == 0 {
		add("VPCID and SubnetIDs are required")
	}

	if a.Port < 1 || a.Port > 65535 {
		add("Port %d: want the listener port from the port registry", a.Port)
	}

	if len(a.Sources) == 0 {
		add("Sources is empty: name who may connect (the runners' security group)")
	}

	c := a.Certificate
	if c.Cert == "" || c.Key == "" || c.ClientCA == "" {
		add("Certificate.Cert, Key and ClientCA are required parameter names")
	}

	if !aliasRe.MatchString(c.KeyAlias) {
		add("Certificate.KeyAlias %q: want alias/<name>", c.KeyAlias)
	}

	if a.CacheVolumeGiB < 20 || a.RootVolumeGiB < 8 {
		add("CacheVolumeGiB %d and RootVolumeGiB %d: want at least 20 and 8", a.CacheVolumeGiB, a.RootVolumeGiB)
	}

	return errors.Join(errs...)
}

// New creates one builder. It refuses an invalid Args before it registers
// anything.
func New(ctx *pulumi.Context, name string, args *Args, opts ...pulumi.ResourceOption) (*Builder, error) {
	if args == nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): args is nil", name)
	}

	a := args.withDefaults(name)
	if err := a.validate(); err != nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): %w", name, err)
	}

	names := asg.Names{Base: a.Name}
	resolved := names.Resolve(name)

	rendered, mods, err := userData(a, resolved)
	if err != nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): %w", name, err)
	}

	b := &Builder{Rendered: rendered}

	f, err := front.Attach(ctx, name, a.NLB, []front.Listener{{
		Name:                       a.Listener,
		Port:                       a.Port,
		TargetPort:                 DaemonPort,
		IdleTimeoutSeconds:         DefaultIdleTimeoutSeconds,
		DeregistrationDelaySeconds: DefaultDeregistrationDelaySeconds,
		Sources:                    a.Sources,
	}}, opts...)
	if err != nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): %w", name, err)
	}

	b.Front = f

	box, err := asg.New(ctx, name, &asg.Args{
		Names:    names,
		Tags:     a.Tags,
		Provider: a.Provider,
		UserData: rendered,
		Image:    asg.ImageArgs{SSMParameter: imageParameters[a.Arch]},
		Instance: asg.InstanceArgs{
			Type:       a.InstanceType,
			HopLimit:   1,
			RootVolume: &asg.RootVolumeArgs{SizeGiB: a.RootVolumeGiB},
			Volumes:    []asg.VolumeArgs{{DeviceName: DefaultCacheDevice, SizeGiB: a.CacheVolumeGiB}},
		},
		Group: asg.GroupArgs{
			Min: a.Min, Max: a.Max, Desired: a.Desired,
			SubnetIDs:           a.SubnetIDs,
			HealthCheckType:     "ELB",
			TargetGroupARNs:     f.TargetGroupARNs(),
			TerminationPolicies: []string{"NewestInstance"},
		},
		Hook:     &asg.HookArgs{HeartbeatTimeoutSeconds: DefaultHookHeartbeatSeconds},
		WarmPool: &asg.WarmPoolArgs{MinSize: *a.WarmPoolSize, ReuseOnScaleIn: true},
		Refresh:  &asg.RefreshArgs{MinHealthyPercentage: 100, MaxHealthyPercentage: 200},
		IAM: asg.IAMArgs{
			PermissionsBoundary: a.PermissionsBoundary,
			StatementsFunc:      func(pc asg.PolicyContext) []asg.Statement { return []asg.Statement{decrypt(pc, a.Certificate)} },
		},
		Security: asg.SecurityArgs{
			VPCID:       a.VPCID,
			Description: a.Name + " BuildKit builder",
			Style:       asg.RulesSeparate,
			Ingress:     f.TargetIngress(),
		},
		Contribution: mods,
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): %w", name, err)
	}

	b.Box = box

	return b, nil
}

// userData composes the daemon, the certificate and the watchdog.
func userData(a Args, r asg.Resolved) (*bootstrap.Rendered, asg.Contribution, error) {
	cert, key, ca := ssmcert.Args{Name: "buildkit"}.Files()

	d := a.Daemon
	d.TLS = buildkit.TLS{Cert: cert, Key: key, CA: ca}
	d.CacheDevice = DefaultCacheDevice
	d.Port = DaemonPort

	rot := a.Rotation
	rot.Name = "buildkit"
	rot.Cert, rot.Key, rot.CA = a.Certificate.Cert, a.Certificate.Key, a.Certificate.ClientCA
	rot.Owner = buildkit.User + ":" + buildkit.User
	rot.RestartUnits = []string{buildkit.Unit}
	rot.ReadyCheck = buildkit.ReadyCheck()
	rot.DrainPort = DaemonPort
	rot.Group, rot.Hook = r.Group, r.Hook
	rot.InPlace = false

	wd := watchdog.Args{IntervalSeconds: 30, FailMinutes: 3, RestartAttempts: 2}
	if a.Watchdog != nil {
		wd = *a.Watchdog
	}

	wd.Checks = append(wd.Checks, buildkit.WatchCheck())
	wd.RestartUnits = []string{buildkit.Unit}

	mods, err := asg.Collect(buildkit.New(d), ssmcert.New(rot), watchdog.New(wd))
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	p, err := bootstrap.New(bootstrap.Options{Lifecycle: r.Lifecycle()})
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	if err := mods.AddTo(p); err != nil {
		return nil, asg.Contribution{}, err
	}

	rendered, err := p.Render()
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	return rendered, mods, nil
}

// decrypt is the builder's own grant: kms:Decrypt of the key behind the
// alias, through SSM, for the three parameters only.
func decrypt(pc asg.PolicyContext, c Certificate) asg.Statement {
	params := make([]string, 0, 3)
	for _, p := range []string{c.Cert, c.Key, c.ClientCA} {
		params = append(params, asg.ParameterARN(pc.Partition, pc.Region, pc.Account, p))
	}

	return asg.Statement{
		Sid:      "DecryptTheCertificate",
		Effect:   "Allow",
		Action:   []string{"kms:Decrypt"},
		Resource: []string{fmt.Sprintf("arn:%s:kms:%s:%s:key/*", pc.Partition, pc.Region, pc.Account)},
		Condition: map[string]map[string][]string{
			"StringEquals": {
				"kms:ViaService":                      {"ssm." + pc.Region + ".amazonaws.com"},
				"kms:EncryptionContext:PARAMETER_ARN": params,
			},
			"ForAnyValue:StringEquals": {"kms:ResourceAliases": {c.KeyAlias}},
		},
	}
}

// KeyPolicyStatement is the builder's part of the policy of the key behind
// Certificate.KeyAlias: kms:Decrypt by the builder's role (the role New
// creates is named Name), through SSM, for the builder's three parameters
// only, the same three as the role's own grant. The principal is the
// account, narrowed by aws:PrincipalArn, so the key can be created before
// the role exists and the statement survives the role being replaced. The
// key and the rest of its policy are the caller's; a key with several
// builders (one per architecture) takes one statement per builder, so no
// builder can decrypt another's key.
func KeyPolicyStatement(partition, region, account, name string, c Certificate) (map[string]any, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("key policy statement: name %q: want lower-case letters, digits and -, at most 48", name)
	}

	if c.Cert == "" || c.Key == "" || c.ClientCA == "" {
		return nil, errors.New("key policy statement: Certificate.Cert, Key and ClientCA are required parameter names")
	}

	if partition == "" || region == "" || account == "" {
		return nil, errors.New("key policy statement: partition, region and account are required")
	}

	params := make([]string, 0, 3)
	for _, p := range []string{c.Cert, c.Key, c.ClientCA} {
		params = append(params, asg.ParameterARN(partition, region, account, p))
	}

	return map[string]any{
		"Sid":       "Builder" + sidWord(name) + "DecryptsItsCertificate",
		"Effect":    "Allow",
		"Principal": map[string]any{"AWS": fmt.Sprintf("arn:%s:iam::%s:root", partition, account)},
		"Action":    []string{"kms:Decrypt"},
		"Resource":  "*",
		"Condition": map[string]any{
			"ArnEquals": map[string]any{"aws:PrincipalArn": fmt.Sprintf("arn:%s:iam::%s:role/%s", partition, account, name)},
			"StringEquals": map[string]any{
				"kms:ViaService":                      "ssm." + region + ".amazonaws.com",
				"kms:EncryptionContext:PARAMETER_ARN": params,
			},
		},
	}, nil
}

// sidWord is a name as one statement-id word: devel-buildkit-arm64 is
// DevelBuildkitArm64.
func sidWord(name string) string {
	var b []byte

	up := true

	for i := 0; i < len(name); i++ {
		ch := name[i]

		switch {
		case ch == '-':
			up = true
		case up && ch >= 'a' && ch <= 'z':
			b = append(b, ch-'a'+'A')
			up = false
		default:
			b = append(b, ch)
			up = false
		}
	}

	return string(b)
}
