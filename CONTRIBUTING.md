# Contributing

AgentsPool deliberately keeps the control plane dependency-free. New production dependencies should be justified against binary size, cross-compilation, router compatibility and failure recovery.

Before submitting a change, run:

```bash
make fmt
make check
make race
make e2e
```

Keep API changes backward-compatible within the v1 namespace. Add tests for state transitions and failure semantics, especially lease expiry and non-idempotent execution. Never log bearer tokens, lease tokens, task secrets or the contents of a Worker secrets file.

Pull requests should explain the user-visible behavior, compatibility impact and verification performed. Generated release files under `dist/` do not belong in Git. Do not attach real `state.json`, Worker credentials, logs, environment files, LAN addresses or Artifacts to an issue; reproduce with synthetic data instead.

Security vulnerabilities must follow [SECURITY.md](SECURITY.md) and must not be disclosed in a public issue.
