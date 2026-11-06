"""The README quick start is examples/quickstart.py, which CI runs on the KVM host."""

import ast
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]


def test_readme_quick_start_is_the_example():
    readme = (ROOT / "README.md").read_text()
    block = re.search(r"```python\n(.*?)```", readme, re.S).group(1)
    example = (ROOT / "examples" / "quickstart.py").read_text()
    tree = ast.parse(example)
    body = example.split('"""', 2)[2].lstrip("\n") if ast.get_docstring(tree) else example
    assert block == body
