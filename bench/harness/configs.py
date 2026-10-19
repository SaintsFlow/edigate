"""The configurations of the protocol (section 2) and the tools each task may use (section 4).

Input: a configuration id (C0..C3, A1..A4) and a document set (850 or 810).
Output: the gateway mode, whether the gateway tables and the relay exist in that mode, and the list of
tool names the agent is offered, cut by the tools/list of the running mode.

C2 is `plain` behind agentgateway. It is available only when the proxy is up (see stand.py and
bench/agentgateway/README.md); otherwise `check_ready` raises an error that names the reason.
"""
import subprocess
from dataclasses import dataclass


@dataclass(frozen=True)
class Config:
    id: str
    mode: str          # value of --mode of the gateway
    label: str


CONFIGS = {c.id: c for c in (
    Config("C0", "plain", "plain MCP"),
    Config("C1", "reqhash", "request-hash key"),
    Config("C2", "plain", "plain behind agentgateway"),
    Config("C3", "full", "edigate"),
    Config("A1", "no-sanitize", "C3 without the sanitizer"),
    Config("A2", "no-policy", "C3 without the policy"),
    Config("A3", "no-bizkey", "C3 without the business key"),
    Config("A4", "no-contracts", "C3 without contracts"),
)}

# Modes with the outbox, relay, sagas and receipts. C0, C1 and C2 call the mocks at once.
GATEWAY_MODES = {"full", "no-sanitize", "no-policy", "no-bizkey", "no-contracts"}

# Tools of the tasks (protocol section 4). `edi.extract_note` is added where tools/list offers it.
TOOLS_850 = ["edi.list_inbox", "edi.read_document", "erp.create_sales_order", "erp.confirm_order", "wms.reserve",
             "edi.send_asn", "notify.send_email", "txn.status", "erp.get_price_list"]
TOOLS_810 = ["edi.list_inbox", "edi.read_document", "erp.post_vendor_invoice", "txn.status", "erp.get_price_list"]
# Every tool the agent has in C3 (both tasks and the note reader). The authorization rule of agentgateway in C2
# lists these and no more.
C3_TOOLS = sorted(set(TOOLS_850 + TOOLS_810 + ["edi.extract_note"]))

AG_IMAGE = "ghcr.io/agentgateway/agentgateway:v1.6.0"


def get(config_id):
    if config_id not in CONFIGS:
        raise ValueError(f"unknown configuration {config_id!r} (want one of {', '.join(CONFIGS)})")
    return CONFIGS[config_id]


def has_gateway_tables(config_id):
    """True where outbox, approvals and sagas exist (the oracle of approval bypass needs them)."""
    return get(config_id).mode in GATEWAY_MODES


def is_open(config_id):
    """True for C0, C1, C2: no relay, no sagas, no receipts."""
    return not has_gateway_tables(config_id)


def _image_present():
    """True if the pinned agentgateway image is on this machine."""
    return subprocess.run(["docker", "image", "inspect", AG_IMAGE], capture_output=True).returncode == 0


def check_ready(config_id):
    """Raises RuntimeError with the reason if the configuration cannot run here."""
    if config_id == "C2" and not _image_present():
        raise RuntimeError(f"C2 needs agentgateway: pull the image once with `docker pull {AG_IMAGE}` "
                           "(bench/agentgateway/README.md); until then C2 does not run")


def offered_tools(set_id, listed):
    """The tool names of the task that the running mode lists, in the order of the protocol."""
    wanted = TOOLS_850 if set_id == "850" else TOOLS_810
    names = list(wanted)
    if "edi.extract_note" in listed:
        names.append("edi.extract_note")
    return [n for n in names if n in listed]
