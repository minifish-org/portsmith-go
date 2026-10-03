# Security

Portsmith Go is experimental. There is no production-hardening or supported-version guarantee yet.

Pith's agent file and shell tools execute with the local user's permissions. A working directory is not an OS sandbox. Treat source files, model responses and tool output as untrusted. Use an isolated environment for untrusted workloads, with credentials and network access restricted outside the application.

Frozen inputs, independent judges and transaction hashes protect migration correctness and recovery. They do not sandbox model-controlled shell commands or prove generated code is safe to deploy.

Keep API keys, `.env` files, raw sessions, private source code and tool output out of issues and commits. `.portsmith/` may contain sensitive prompts, source snapshots, reports and tool results. Redact diagnostics before sharing them.

Report a suspected vulnerability privately to the maintainer at i@minifish.org, with a minimal reproduction and affected revision. Do not post exploit details or secrets in a public issue. No response-time SLA is promised.
