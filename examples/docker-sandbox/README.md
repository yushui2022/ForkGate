# Docker backend compatibility smoke

This directory is a local backend compatibility smoke for the ForkGate demo. It
uses Docker to create a parent container, copies its filesystem with
`docker commit`, starts three child containers, and then registers those already
created children with ForkGate. ForkGate does not create or copy the sandbox.

The containers are real, but this is not an enterprise Agent Sandbox such as
CubeSandbox: `docker commit` copies a filesystem only, shares the Docker host
kernel, and does not restore a running process or memory checkpoint. Use this
adapter to check ForkGate's branch and side-effect contract; use a microVM or
managed sandbox backend for production isolation and fast clone/restore.

Run from PowerShell after starting Docker Desktop's Linux engine:

```powershell
& .\examples\docker-sandbox\run.ps1
```

The workload uses an explicit proxy inside real containers. Expected output is
`child1 after_abort=410`, `commit=committed`, `after_commit=410`, and two
committed writes at the upstream, while the aborted and losing children never
produce an upstream write. The script uses an isolated user-defined Docker
bridge with localhost-only published ports and opts into non-loopback listeners
inside that network using `FORKGATE_ALLOW_NON_LOOPBACK=1`.

This is the explicit-proxy step. It does not yet prove transparent bypass
blocking; Docker Linux netns/veth/nftables is the next adapter required for
that guarantee.
