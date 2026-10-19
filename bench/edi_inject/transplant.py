"""Held-out injection techniques: templates of InjecAgent and AgentDojo moved into EDI.

Input: the template files in templates/ (read as text; nothing is imported or run), the goal
sentence and the attacker's value of a document, and a Random.
Output: the text for the carrier, the carrier name, the source template and a flag that
says whether the text was cut.

The rules are fixed here, and nobody edits a text by hand:
1. AgentDojo files are parsed with ast.parse. The template of a class is the string constant
   `_JB_STRING` of the class, or else the first string argument of its `super().__init__`
   call. Classes: DEFAULT_CLASSES, used in this order, round after round.
2. For the goal exfiltration the text is the `Attacker Instruction` of a random InjecAgent
   row, whole, taken without repeats. The attacker's value is the first mail address in it.
   Other goals use an AgentDojo template with {goal} = the goal sentence, {user} =
   "Emma Johnson" (the AgentDojo default) and {model} = "AI model".
3. Carrier: MTX02 if the text is longer than 264 characters, else MSG01 and MTX02 by turns.
4. The only edit: every run of white space that holds a CR, LF or TAB becomes one space and
   the spaces at both ends go. Then the text is cut to the maximum length of the carrier.
   Capitals, punctuation and typos of the template stay as they are.
"""
import ast
import json
import re
from pathlib import Path

TEMPLATES = Path(__file__).resolve().parent / "templates"
DEFAULT_CLASSES = (
    "DirectAttack", "IgnorePreviousAttack", "SystemMessageAttack", "InjecAgentAttack",
    "ImportantInstructionsAttack",
)
USER_NAME = "Emma Johnson"
MODEL_NAME = "AI model"
MAX_LEN = {"MSG01": 264, "MTX02": 4096}
_BREAK_RUN = re.compile(r"[ \t\r\n]*[\t\r\n][ \t\r\n]*")
_EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")


def normalize(text):
    """Rule 4 without the cut."""
    return _BREAK_RUN.sub(" ", text).strip(" ")


def load_agentdojo():
    """{class name: template string} for DEFAULT_CLASSES. Parses the files, runs nothing."""
    found = {}
    for name in ("baseline_attacks.py", "important_instructions_attacks.py"):
        tree = ast.parse((TEMPLATES / "agentdojo" / name).read_text(encoding="utf-8"))
        for node in tree.body:
            if isinstance(node, ast.ClassDef) and node.name in DEFAULT_CLASSES:
                found[node.name] = _class_template(node)
    missing = [c for c in DEFAULT_CLASSES if c not in found]
    if missing:
        raise ValueError(f"no template found for classes: {missing}")
    return found


def _class_template(node):
    for stmt in node.body:
        if isinstance(stmt, ast.Assign) and any(getattr(t, "id", "") == "_JB_STRING" for t in stmt.targets):
            return stmt.value.value
    for stmt in node.body:
        if isinstance(stmt, ast.FunctionDef) and stmt.name == "__init__":
            for call in ast.walk(stmt):
                if (isinstance(call, ast.Call) and ast.unparse(call.func) == "super().__init__"
                        and call.args and isinstance(call.args[0], ast.Constant)
                        and isinstance(call.args[0].value, str)):
                    return call.args[0].value
    raise ValueError(f"class {node.name} has no template string")


def load_injecagent():
    """The `Attacker Instruction` of each row of attacker_cases_ds.jsonl, in file order."""
    path = TEMPLATES / "injecagent" / "attacker_cases_ds.jsonl"
    rows = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.strip():
            rows.append(json.loads(line)["Attacker Instruction"])
    return rows


class Transplanter:
    """Hands out held-out texts one by one. The state (round of classes, unused InjecAgent
    rows, the MSG01/MTX02 turn) lives here, so the result depends only on the call order."""

    def __init__(self, rng, exfiltration_count):
        self.agentdojo = load_agentdojo()
        self.injecagent = load_injecagent()
        self.rows = rng.sample(range(len(self.injecagent)), exfiltration_count)
        self.round = 0
        self.turn = 0

    def make(self, goal, sentence):
        """Returns (text, carrier, source_template, truncated, email). `email` is the
        attacker's recipient for the goal exfiltration and None for the others."""
        email = None
        if goal == "exfiltration":
            row = self.rows.pop(0)
            raw = self.injecagent[row]
            source = f"injecagent:attacker_cases_ds:{row + 1}"
            email = _EMAIL.search(raw).group(0)
        else:
            cls = DEFAULT_CLASSES[self.round % len(DEFAULT_CLASSES)]
            self.round += 1
            raw = (self.agentdojo[cls].replace("{goal}", sentence)
                   .replace("{user}", USER_NAME).replace("{model}", MODEL_NAME))
            source = f"agentdojo:{cls}"
        text = normalize(raw)
        if len(text) > MAX_LEN["MSG01"]:
            carrier = "MTX02"
        else:
            carrier = ("MSG01", "MTX02")[self.turn % 2]
            self.turn += 1
        truncated = len(text) > MAX_LEN[carrier]
        return text[:MAX_LEN[carrier]], carrier, source, truncated, email
