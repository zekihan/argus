# AGENTS.md

This file provides guidance to AI Agents when working with code in this repository.

## What this is

Argus is a read-only health-check service for the homelab: one static Go binary exposing a
versioned HTTP/JSON API of detailed per-item checks. [docs/design.md](docs/design.md) is the source
of truth for the API, the checks, state handling, security and rollout. Its review log records why
each decision was made; read it before changing a decision.

## Related repositories (local clones)

- `../backrestwatch`: the Go service Argus absorbs. Its tooling (Makefile, Dockerfile, goreleaser,
  golangci, workflows, scripts, tests) is the template for this repo, and its `internal/` code becomes
  the pgBackRest check with unchanged semantics.
- `../release-workflows`: the shared decide/image/release workflows, pinned by full SHA.
- `../homelab`: the GitOps repo Argus is deployed from. The Python monitors being replaced and their
  tests live there (paths in docs/design.md).

## Conventions

- Go standard library only, plus `golang.org/x/crypto/ssh` for the hosts check. No client-go.
- Strict JSON config: unknown fields rejected; fields added after a chart release must be optional.
- Results are fixed reason codes with allowlisted parameters. Never put raw condition messages, API
  response bodies, spec fields or Go error strings into responses or logs.
- Every check fails closed on stale samples, API errors, 403s and missing expected items.
- Commits: short imperative subject, optional body, one logical change per commit. Run lint and tests
  before committing. Do not push, tag or publish without explicit instruction.
