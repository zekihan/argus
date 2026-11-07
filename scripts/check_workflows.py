#!/usr/bin/env python3
"""Offline policy for action pins, checkout credentials, and publication authority."""
from pathlib import Path
import re
import sys
import yaml

PIN = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_./-]+)?@[0-9a-f]{40}")


def validate(document):
    errors = []
    def visit(value, location="workflow"):
        if isinstance(value, dict):
            if "uses" in value:
                ref = value["uses"]
                if not isinstance(ref, str) or not (ref.startswith("./") or PIN.fullmatch(ref)):
                    errors.append(f"{location}: action must use a full commit SHA: {ref}")
                if isinstance(ref, str) and ref.startswith("actions/checkout@"):
                    if value.get("with", {}).get("persist-credentials") != "false":
                        errors.append(f"{location}: checkout must disable persisted credentials")
            for key, child in value.items():
                visit(child, f"{location}.{key}")
        elif isinstance(value, list):
            for index, child in enumerate(value):
                visit(child, f"{location}[{index}]")
    visit(document)
    permissions = document.get("permissions", {})
    if not isinstance(permissions, dict) or any(v != "read" and v != "none" for v in permissions.values()):
        errors.append("workflow: write authority must be job-scoped")
    def secrets_in(env):
        return re.search(r"secrets\s*[.\[]", str(env)) is not None
    if secrets_in(document.get("env", {})):
        errors.append("workflow: secrets must be step-scoped")
    for name, job in document.get("jobs", {}).items():
        if secrets_in(job.get("env", {})):
            errors.append(f"{name}: secrets must be step-scoped")
        perms = job.get("permissions", {})
        if not isinstance(perms, dict):
            errors.append(f"{name}: permissions must be explicit")
        else:
            for scope, level in perms.items():
                if level == "write" and (name not in {"publish", "release"} or scope not in {"contents", "packages"}):
                    errors.append(f"{name}: unexpected write authority for {scope}")
    return errors


def main():
    root = Path(__file__).resolve().parents[1]
    failures = []
    paths = sorted((root / ".github/workflows").glob("*.y*ml"))
    for path in paths:
        try:
            document = yaml.load(path.read_text(), Loader=yaml.BaseLoader)
            failures.extend(f"{path.name}: {error}" for error in validate(document))
        except (yaml.YAMLError, AttributeError, TypeError) as error:
            failures.append(f"{path.name}: invalid workflow: {error}")
    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(f"Workflow policy passed ({len(paths)} workflows)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
