// Package ec2 runs a BuildKit builder (package buildkit) on EC2: ONE plain
// instance per architecture (asg Args.Single) that is stopped when nobody
// builds, with its cache on a persistent encrypted data volume (asg
// Args.Data, mounted by the datavolume module at the daemon's state
// directory), behind a wake proxy ([NewProxy], asg's cmd/wake-proxy) that
// owns its run state: it starts the instance when a client connects, holds
// the connection until the daemon answers, pipes the TCP stream through
// (mutual TLS stays end to end) and stops the instance after 20 minutes
// without a connection. The shared NLB's listeners and target groups belong
// to the proxy, not to the builders.
//
// What one New call creates (asg.New): a launch template (the image of the
// architecture, IMDSv2 with hop limit 1, an encrypted root), the instance and
// its data volume, the role with exactly the reads its user-data names, and
// a security group that admits the daemon's port from the proxy's group only.
//
// On the box (the user-data): the daemon (package buildkit), the data volume,
// and asg's ssmcert module (the server certificate, its key and the client CA
// from SSM into /run on every boot, rotated in place when idle, since
// buildkitd never reloads its certificate).
//
// The one grant that is the builder's own: kms:Decrypt of the certificate
// parameters' key, through SSM, for those parameters only.
//
// A single instance has no launch lifecycle (docs/buildkit-on-ec2.md): the
// certificate is fetched on every boot and rotated in place when idle, and
// there is no watchdog, Standby rotation, ready gate or launch hook. The
// group mode of earlier releases (an Auto Scaling group with a warm pool,
// launch hook and watchdog per builder) was removed in v1.0.0.
package ec2

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/modules/ssmcert"

	"github.com/truvity/ci-support/pkg/buildkit"
)

// Defaults.
const (
	DefaultRootVolumeGiB = 20
	DefaultCacheDevice   = "/dev/sdf"
	// DefaultIdleTimeoutSeconds is the listener's TCP idle timeout: a build
	// step can be silent for a long time.
	DefaultIdleTimeoutSeconds = 3600
	// DefaultDeregistrationDelaySeconds is how long a leaving instance keeps
	// serving the connections it has. 300 fits the AWS provider's ~10 minute
	// wait for an Auto Scaling Group to drain, so deleting a group succeeds
	// and an instance refresh does not stall 15 minutes per instance.
	DefaultDeregistrationDelaySeconds = 300
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

// Mode says how a builder runs. On demand is the only mode (the group mode
// was removed in v1.0.0); the type stays so that a caller that names it, or a
// configuration file with `mode: on-demand`, keeps working.
type Mode string

// ModeOnDemand is one plain instance with a persistent cache volume, started
// and stopped by a wake proxy (see [NewProxy] and the package documentation).
// It is also the empty Mode.
const ModeOnDemand Mode = "on-demand"

type (
	// Args configure one builder (one architecture).
	Args struct {
		// Mode is ModeOnDemand, the only mode; empty means the same.
		Mode Mode
		// OnDemand holds the settings of the builder. Required.
		OnDemand *OnDemandArgs

		// Arch is amd64 or arm64. Required.
		Arch string
		// Name is the physical name of the instance, its launch template, role
		// and security group prefix (at most 48 characters), such as
		// devel-buildkit-arm64. Default: the logical name.
		Name string
		// InstanceType must be of the architecture. Required.
		InstanceType string

		// VPCID places the instance's security group. Required. The
		// instance's subnet is OnDemand.SubnetID.
		VPCID pulumi.StringInput
		// PermissionsBoundary is the role's boundary policy ARN.
		PermissionsBoundary string
		// Tags are added to every resource that takes tags.
		Tags map[string]string
		// Provider is the AWS provider for the lookups.
		Provider *aws.Provider

		// Port is the listener's port, from the caller's port registry.
		// Required. It is the port the NLB listens on and the proxy's listen
		// port for this builder (see ProxyRoute).
		Port int

		// Certificate are the SSM SecureStrings of the server certificate,
		// its key and the client CA, and the alias of the key they are
		// encrypted with. Required.
		Certificate Certificate

		// Daemon configures buildkitd. TLS is set here.
		Daemon buildkit.Config
		// RootVolumeGiB is the root volume. Default 20.
		RootVolumeGiB int

		// Rotation tunes the certificate rotation (asg ssmcert): only the
		// timing; names, parameters, units, port and owner are set here.
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
	// its own: the box (truvity:asg:Box) is registered with the caller's
	// options, the parent of its own resources.
	Builder struct {
		// Box is the single instance (Box.Instance), its data volume
		// (Box.DataVolume) and their parts; there is no group.
		Box *asg.Appliance
		// RoleTagKey and RoleTagValue are the tag the proxy finds the
		// instance by.
		RoleTagKey, RoleTagValue string
		// Rendered is the user-data, for review goldens.
		Rendered *bootstrap.Rendered
	}
)

var (
	aliasRe = regexp.MustCompile(`^alias/[A-Za-z0-9/_-]{1,250}$`)
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
)

// New creates one builder. It refuses an invalid Args before it registers
// anything.
func New(ctx *pulumi.Context, name string, args *Args, opts ...pulumi.ResourceOption) (*Builder, error) {
	if args == nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): args is nil", name)
	}

	switch args.Mode {
	case "", ModeOnDemand:
		return newOnDemand(ctx, name, args, opts...)
	default:
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): Mode %q: want %q (the only mode)", name, args.Mode, ModeOnDemand)
	}
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
