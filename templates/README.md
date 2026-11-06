# Templates

Template specs for `forkbox templates build`:

- `python.yaml`: Python 3.12, git, build tools, numpy and pytest; the warm-up imports numpy.
- `node.yaml`: Node.js 22 LTS with git and npm.

Spec fields: `name`, `base` (an OCI image reference; a `@sha256:` digest pins
it), `apt`, `pip`, `files` (`src` relative to the spec, absolute `dst`,
optional octal `mode`), `env`, `warm_up` (shell commands run in the booted VM
before the base snapshot), `vcpus`, `memory_mib`, `disk_size_mib`.

A template's id hashes the spec (copied files by content), `versions.yaml`
and the guest agent binary, so a change to any of them builds a new
template. A tag such as `python:3.12-slim` is resolved at build time and
recorded in `template.json`; it is not re-resolved for an unchanged spec.
