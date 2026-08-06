# Kanedias Repository Rename Design

## Goal

Make Kanedias the canonical project identity throughout every tracked project file and publish the repository as a single root commit on `main`.

## Scope

Apply a case-preserving identity replacement across tracked source code, tests, shell scripts, configuration, Go module metadata, and existing documentation. This includes:

- the Go repository/module name under `github.com/sklarsa`;
- the managed Linux user and home directory;
- configuration and certificate paths;
- sandbox image, lock, and fixture names;
- public environment variables;
- Prometheus metric namespaces;
- human-readable project descriptions.

No tracked path currently requires a filename rename. Git internals and ignored generated `.pi-subagents/artifacts` are outside the rename scope. The repository must remain without a configured remote.

## Implementation

Perform a deterministic, case-preserving replacement in all tracked text files. Validate the resulting tree before rewriting history. Build a new parentless commit directly from the final tree, repoint `main` to it, and remove any temporary branch or reference used during the operation.

## Validation

- Search all tracked files case-insensitively for the retired identity and require zero matches.
- Run the complete Go test suite.
- Run shell syntax checks for every tracked shell script.
- Run applicable repository shell test suites.
- Confirm `main` contains exactly one commit.
- Confirm the working tree is clean and no Git remote is configured.
