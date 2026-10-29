# Mutual TLS for the BuildKit builders

The builders are `pkg/buildkit/ec2`'s: one on-demand instance per
architecture, outside the cluster ([buildkit-on-ec2.md](buildkit-on-ec2.md)).
They serve mutual TLS on :1234 and accept a client only when its certificate
chains to the ONE client CA. This chart issues the trust and the certificates;
it never runs a builder. (The in-cluster `buildkitd` StatefulSets, their
server certificates, NetworkPolicies and the `parallel` / `inPlace` modes were
removed in v1.0.0, see [CHANGELOG.md](../CHANGELOG.md). In-cluster runners got
theirs from the `arc-runners` chart's `buildkitTLS`; that chart is archived,
last released as 7.8.3.)

## The pieces

| What | Where | Notes |
|---|---|---|
| Client CA | `buildkitd.mtls.clientCA` | The only authority the builders accept clients from. `create: true` builds it: self-signed Issuer and CA Certificate in cert-manager's namespace, a CA `ClusterIssuer`, an approver-policy `CertificateRequestPolicy` (one common name, client auth, ECDSA, listed namespaces), and a trust-manager `Bundle` publishing the CA certificate, never its key, to the ConfigMap `clientCA.configMapName`. |
| Client certificate | `buildkitd.mtls.clientCA.externalClients` | One `Certificate` per client name, delivered as `ca.pem`, `cert.pem`, `key.pem`; see [Clients outside the cluster](#clients-outside-the-cluster). |
| Server certificate | `externalBuilders` | One per architecture, pushed to a store the machines read; see [Builders outside the cluster](#builders-outside-the-cluster). |

`buildkitd.enabled` (default true) and `buildkitd.mtls.enabled` (default
false) gate the client CA and the client certificates; `externalBuilders` has
its own switch. Any other key under `buildkitd` fails the render with the
release that removed it.

The client CA is dedicated on purpose. Reusing a general private CA would
admit any certificate it issues for any other workload.

## Renewal and rotation

- **Client certificates** are read by a job when it starts. A renewal reaches
  the next job and breaks nothing in flight. The default is 90 days with
  renewal 30 days before expiry.
- **The client CA** lives ten years and its key is never rotated by
  cert-manager (`rotationPolicy: Never`). Replacing it is a new issuer name,
  both CAs in the ConfigMap for a while, then the old one dropped.
- **Server certificates** are loaded by `buildkitd` once, at start. The EC2
  builders fetch them on every boot and rotate in place when idle
  (buildkit-on-ec2.md, "On demand").

## Clients outside the cluster

A runner outside Kubernetes (a serverless task, a VM) cannot mount the
Secret. It gets its own client identity instead of a copy of the in-cluster
runners', and the three files (`ca.pem`, `cert.pem`, `key.pem`) reach it from
a store the estate owns. `buildkitd.mtls.clientCA.externalClients` renders
both halves, per client name:

- a cert-manager `Certificate` in the release namespace, from the client CA's
  `ClusterIssuer`, with the entry's **own common name** (not `clientCA.commonName`;
  the chart refuses the runners' name, and two entries with one name);
- a `CertificateRequestPolicy` that admits exactly that common name, from
  exactly the release namespace, plus the `use` grant for cert-manager there.
  The in-cluster runners' policy is not bound in this namespace, and this one
  is bound nowhere else, so neither identity can ask for the other's name. The
  release namespace must not be in `requesterNamespaces`;
- with `push.enabled`, an External Secrets `PushSecret` that writes `ca.pem`
  (`push.caPem`, the bundle the client trusts the builders' server
  certificate by), `cert.pem` (`tls.crt`) and `key.pem` (`tls.key`) to the
  store named by `push.storeRef`, as three values named by `push.names`.
  `kmsKeyId` and `tags` go to the provider (Parameter Store `SecureString`,
  for one). It never deletes from the store (`deletionPolicy: None`).

The builders do not change: they verify the chain to the one client CA, and
nothing else about the client. Who is issued a certificate is the policy's
decision, so a new identity is a new entry, reviewed as one. The estate owns
what the chart cannot know: the store (and its credentials and prefix), the
key that encrypts the values, who may read them, and the consumer that writes
the three files into one directory and sets `CI_BUILDKIT_TLS_DIR`.

Renewal needs no human: cert-manager renews the Secret `renewBefore` ahead
of expiry, the `PushSecret` pushes when the Secret changes and every
`refreshInterval`, and a client that starts after that reads the new pair. A
client in flight keeps the one it read, which is the damage window of the
runners' certificate as well. Set `refreshInterval` well under `renewBefore`.

## Builders outside the cluster

A builder on a machine of its own (`pkg/buildkit/ec2`, [buildkit-on-ec2.md](buildkit-on-ec2.md))
cannot mount a Secret either. It reads its server certificate, its key and the
client CA from a store the estate owns, and `externalBuilders` puts them
there. It is independent of `buildkitd.enabled`, so a release may render it
alone:

- per architecture in `externalBuilders.archs`, a cert-manager `Certificate`
  (`buildkitd-external-<arch>`, Secret `buildkitd-external-<arch>-tls`) for
  `serverCertificate.dnsNames.<arch>`, from `serverCertificate.issuerRef`.
  Name the issuer your clients' CA bundle chains to (the Fargate runners hold
  its private roots), and list every name a client dials, so that a name can
  move between machines without a new certificate;
- per architecture, a `PushSecret` that writes `tls.crt` (with its chain) and
  `tls.key` as `<arch>/<push.names.cert>` and `<arch>/<push.names.key>`;
- one `PushSecret` that writes the client CA certificate as
  `push.names.clientCA`.

The client CA is read from `clientCA.secretName`, a Secret in the release
namespace, key `clientCA.key` (`ca.crt`). A `PushSecret` reads only Secrets,
and only those of its own namespace: the CA's own Secret is in
`buildkitd.mtls.clientCA.namespace`, and the trust-manager `Bundle` publishes a
ConfigMap. Every certificate the client CA issues carries it in `ca.crt`, so
the Secret of an external client (`buildkit-client-<name>-tls`) is such a
Secret, and the Secret of the client that dials these builders is the
closest choice: what the machines trust is then by construction the issuer of
that client's certificate. Only `clientCA.key` is pushed from it, never its
pair.

Renewal: cert-manager renews `renewBefore` ahead of expiry, the `PushSecret`
pushes on the change and every `refreshInterval`, and the machine picks the
new parameter version up on its next boot, or in place when idle (see
[buildkit-on-ec2.md](buildkit-on-ec2.md)). Nothing is deleted from the
store when an object goes (`deletionPolicy: None`).

## What this does not do

Per-job certificates (federated identity, a short-lived client certificate
per job) are the later step; this gives every runner one certificate.
