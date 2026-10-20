# agentgateway for configuration C2

C2 is the plain gateway (mode `plain`) behind a general MCP gateway that knows nothing about the business: it shows what
a gateway without logic of our own does. Version pinned for the pilot: agentgateway v1.6.0 (released 2 October 2026,
Apache-2.0), image `ghcr.io/agentgateway/agentgateway:v1.6.0`.

`agentgateway.yaml` is the rule file. One container per slot (`bench-ag-<N>`, host port 18300 + N) forwards to the
gateway process of the slot. Its authorization rule lists the tools the agent has in C3 and nothing else (the harness builds the
rule from `harness/configs.py`), so the difference between C0 and C2 is the proxy alone. The container is started by the
harness the first time a C2 episode needs it and removed with the slot. Pull the image once: `docker pull
ghcr.io/agentgateway/agentgateway:v1.6.0`; without the image C2 reports the reason and does not run.

Checked on the stand (19 October 2026): `tools/list` through the proxy shows only the allowed tools, a call to another
tool answers `Unknown tool`, and the token of the agent reaches the gateway.
