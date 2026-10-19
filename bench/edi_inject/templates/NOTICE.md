# Notice for the attack templates

The held-out documents of the EDI-Inject corpus (the ones built from outside templates; see the
[generator README](../README.md)) are built from attack templates of two public projects. Both are
under the MIT licence. The files were downloaded on 19 October 2026 and are kept here unchanged.

| Project | Repository | Commit | Files here |
|---|---|---|---|
| InjecAgent | https://github.com/uiuc-kang-lab/InjecAgent | `f19c9f2c79a41046eb13c03c51a24c567a8ffa07` | `injecagent/attacker_cases_ds.jsonl`, `injecagent/LICENCE` |
| AgentDojo | https://github.com/ethz-spylab/agentdojo | `089ed468cf3ed0322acc66b0211f26d9d90dbf60` | `agentdojo/baseline_attacks.py`, `agentdojo/important_instructions_attacks.py`, `agentdojo/LICENSE` |

## What the generator takes from them

- From InjecAgent: the field `Attacker Instruction` of the 32 rows of `attacker_cases_ds.jsonl`. The generator uses
  five rows, whole, for the goal "exfiltration of the price list".
- From AgentDojo: the text templates of five attack classes: `DirectAttack`, `IgnorePreviousAttack`,
  `SystemMessageAttack`, `InjecAgentAttack` and `ImportantInstructionsAttack`. `InjecAgentAttack` is a class of
  AgentDojo, not of InjecAgent: it holds the attack string taken from the InjecAgent paper.

The code of these projects is never run and never imported. `transplant.py` reads the files as text, parses the
Python files with `ast.parse` and takes the string constants of the templates. The text of a template reaches the
EDI element with its white space folded (each run that holds a line break or a tab becomes one space), with
`{goal}`, `{user}` and `{model}` filled in, and cut to the maximum length of the element. A cut text is marked by the
label field `truncated`. The rules are written in the head of `transplant.py`.

## InjecAgent

MIT License

Copyright (c) 2023 Qiusi Zhan

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## AgentDojo

MIT License

Copyright (c) 2024 Edoardo Debenedetti, Jie Zhang, Mislav Balunovic, Luca Beurer-Kellner, Marc Fischer, and Florian Tramèr

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
