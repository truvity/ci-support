# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via
[GitHub Security Advisories](https://github.com/truvity/ci-support/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

## What is in scope

This repository is the home of the CI services every runner uses: the
build caches, the docker and nix builders and the Helm chart that deploys
them. The code arrives from truvity/ci-plane in a later migration; until
then the repository holds only its scaffolding, and what is in scope is
what it will publish.

Reports that matter most:

- A build cache or read-through cache that can be poisoned by a pull request, or that serves one tenant's artifact to another.
- A builder that accepts a login, a host key or a certificate it should refuse, or that keeps a credential between jobs.
- A container default or a chart value that lets a job escape its sandbox or reach a neighbour, the node or the cluster.
- A secret or token reaching an image layer, a log line or a rendered manifest.

A finding that depends on how a particular deployment uses this repository
belongs with that deployment's owner.
