# Examples

`examples/tasks/` and `examples/python/submit_task.py` demonstrate the compatibility Task API. They are useful for deterministic batch work and Task API regression tests, but they are not the Agent integration contract.

An Agent should discover `.agents/skills/agentspool-compute/SKILL.md` and call `agentspool tool` for Device observation, ResourceLease admission, persistent Sandbox operations and file transfer.

Examples use synthetic identifiers and secrets. Provider credentials, Prompt, context and Agent Session data must remain outside AgentsPool.
